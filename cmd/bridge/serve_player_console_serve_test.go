package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/library"
	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// goBarSerial is goBarPlayer's serial, which no answer of the console's may
// carry.
const goBarSerial = "GOBAR-7Q2X-91KZ-4471"

// goBarOff is goBarPlayer turned off: the GO bar bound, the player not
// enabled.
const goBarOff = "    enabled: false\n    device:\n      binding: serial\n      vendorId: \"20b1\"\n      productId: \"3009\"\n      serial: GOBAR-7Q2X-91KZ-4471\n"

// consoleServe is a serve booted with its player, and its console's address.
type consoleServe struct {
	*playerServe
	console string
}

// bootConsoleServe boots c and keeps the console listener's address.
func bootConsoleServe(t *testing.T, c playerServeCase) *consoleServe {
	t.Helper()
	var addr string
	with := c.with
	c.with = func(o *serveOpts) {
		addr = o.adminListener.Addr().String()
		if with != nil {
			with(o)
		}
	}
	ps := bootPlayerServe(t, c)
	return &consoleServe{playerServe: ps, console: addr}
}

// call sends one request to the console as its page does, on a transport of
// its own, and decodes the answer into out when it is not nil; it answers the
// status and the body.
func (cs *consoleServe) call(t *testing.T, method, path, body string, out any) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://"+cs.console+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), goBarSerial) || strings.Contains(string(raw), goBarSerial[:8]) {
		t.Fatalf("%s %s answered the serial: %s", method, path, raw)
	}
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
	return resp.StatusCode, string(raw)
}

// status is GET /api/output/player.
func (cs *consoleServe) status(t *testing.T) admin.PlayerStatus {
	t.Helper()
	var st admin.PlayerStatus
	if code, body := cs.call(t, http.MethodGet, "/api/output/player", "", &st); code != http.StatusOK {
		t.Fatalf("GET /api/output/player = %d: %s", code, body)
	}
	return st
}

// change sends a write and requires it saved (restart) or unchanged.
func (cs *consoleServe) change(t *testing.T, path, body, want string) consoleChange {
	t.Helper()
	var resp consoleChange
	if code, raw := cs.call(t, http.MethodPost, path, body, &resp); code != http.StatusOK || resp.Status != want {
		t.Fatalf("POST %s %s = %d %q: %s; want %s", path, body, code, resp.Status, raw, want)
	}
	return resp
}

// consoleChange is a write's answer as the page reads it.
type consoleChange struct {
	Status      string             `json:"status"`
	Interrupted bool               `json:"interrupted"`
	Player      admin.PlayerStatus `json:"player"`
}

// dacRow is the console's DAC list's row for card 5, the GO bar.
func (cs *consoleServe) dacRow(t *testing.T) admin.PlayerDACCard {
	t.Helper()
	var list admin.PlayerDACList
	if code, body := cs.call(t, http.MethodGet, "/api/output/player/dacs", "", &list); code != http.StatusOK || !list.Read {
		t.Fatalf("GET /api/output/player/dacs = %d: %s", code, body)
	}
	for _, r := range list.Cards {
		if r.Card == 5 {
			return r
		}
	}
	t.Fatalf("the console lists no card 5: %+v", list.Cards)
	return admin.PlayerDACCard{}
}

// onDisk is the player block the config file holds now.
func (cs *consoleServe) onDisk(t *testing.T) config.PlayerConfig {
	t.Helper()
	cfg, err := config.Load(cs.configPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Output.Player
}

// pendingChanges are the words of what waits for a restart.
func pendingChanges(st admin.PlayerStatus) []string {
	var out []string
	for _, p := range st.Pending {
		out = append(out, p.Change)
	}
	return out
}

// TestTheConsoleShowsTheRunningPlayer pins the Player page's status against a
// real serve: the player it started from the boot snapshot, the binding by
// its kind and its serial's label, the DAC the watcher resolved, the track
// heard named from the library's own row (its path the index stores) with the
// decoder's report, the three transports, and nothing pending; no answer
// carries the serial.
func TestTheConsoleShowsTheRunningPlayer(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	const rel = "Artist/Album (2020)/01 Song.flac"
	standIn := newPlayerStandIn()
	standIn.details = &engine.Details{Frames: 96000 * 200, RateHz: 96000, Bits: 24, Channels: 2, Codec: "FLAC", Lossless: true}
	cs := bootConsoleServe(t, playerServeCase{player: goBarPlayer, standIn: standIn, files: map[string][]byte{rel: []byte("a track")}})
	eng := cs.runningEngine(t)
	availableOutput(t, eng)
	queue(t, eng, 0, library.Reference(rel))
	if err := eng.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitEngine(t, eng, "the track heard", func(s *engine.Snapshot) bool { _, ok := s.Track(); return ok })
	st := cs.status(t)
	run := st.Running
	if !st.Available || !run.Started || run.State != bridgeStatePlaying || run.Transport != "playing" || run.Withheld != "" {
		t.Fatalf("the running player: available %v, %+v", st.Available, run)
	}
	if b := run.Binding; b == nil || b.Kind != config.BindingSerial || b.USBID != "20b1:3009" || b.SerialLabel != usbid.New(goBarSerial).Label() {
		t.Errorf("the boot binding: %+v", run.Binding)
	}
	if d := run.DAC; d == nil || d.State != "available" || d.USBID != "20b1:3009" {
		t.Errorf("the DAC: %+v, want the GO bar, available", run.DAC)
	}
	tr := run.Track
	if tr == nil || !tr.Named || tr.Path != rel || tr.Codec != "FLAC" || tr.RateHz != 96000 || tr.Bits != 24 || tr.Channels != 2 || tr.Duration != 200 {
		t.Errorf("the track heard: %+v, want %s named from the library's row with the decoder's report", tr, rel)
	}
	if len(st.Transports) != 3 || st.Transports[0].Kind != "pcm" || !st.Transports[0].Plays || st.Transports[1].Plays || st.Transports[2].Plays {
		t.Errorf("the transports: %+v, want pcm playing and no DSD", st.Transports)
	}
	if !st.Configured.Enabled || !st.Configured.Starts || len(st.Pending) != 0 {
		t.Errorf("configured %+v, pending %v; want the boot state, nothing pending", st.Configured, st.Pending)
	}
	if code := cs.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
	}
}

// TestTheConsoleTurnsTheRunningPlayerOffAndOnAgain: turned off from the
// console while it plays, the player's output is withheld at once (disabled):
// playback stops, the answer says it was interrupted, and the stop waits for
// the next start; turned on again before a restart, nothing is pending, and
// the output comes back, playing nothing until asked.
func TestTheConsoleTurnsTheRunningPlayerOffAndOnAgain(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	cs := bootConsoleServe(t, playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn()})
	eng := cs.runningEngine(t)
	availableOutput(t, eng)
	queue(t, eng, 0, someReference(t, "0123456789abcdef"))
	if err := eng.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitEngine(t, eng, "playing", func(s *engine.Snapshot) bool { return s.State() == engine.Playing })
	off := cs.change(t, "/api/output/player/enabled", `{"enabled":false}`, "restart")
	if !off.Interrupted || off.Player.Running.Withheld != "disabled" || strings.Join(pendingChanges(off.Player), ",") != "stop" {
		t.Errorf("turning the playing player off: interrupted %v, withheld %q, pending %v; want interrupted, disabled, stop",
			off.Interrupted, off.Player.Running.Withheld, pendingChanges(off.Player))
	}
	awaitEngine(t, eng, "the output withheld, disabled, and playback stopped", func(s *engine.Snapshot) bool {
		return s.Output().Known && !s.Output().Available && s.Output().Reason == "disabled" && s.State() == engine.Stopped
	})
	if cs.onDisk(t).Enabled {
		t.Errorf("the file still turns the player on")
	}
	on := cs.change(t, "/api/output/player/enabled", `{"enabled":true}`, "restart")
	if on.Interrupted || on.Player.Running.Withheld != "" || len(on.Player.Pending) != 0 {
		t.Errorf("turning it on again: interrupted %v, withheld %q, pending %v; want nothing pending", on.Interrupted, on.Player.Running.Withheld, pendingChanges(on.Player))
	}
	s := awaitEngine(t, eng, "the output back", func(s *engine.Snapshot) bool { return s.Output().Available })
	if s.State() != engine.Stopped {
		t.Errorf("the output's return played: %s", s.State())
	}
	if code := cs.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
	}
}

// TestTheConsoleChoosesAnotherDACAndBack: the GO bar chosen by its model while
// the player plays it by its serial is another binding: written, the output
// withheld at once (binding_changed), the change pending; chosen again by its
// serial before a restart, the boot binding is written as the boot snapshot
// held it, nothing is pending, and the output comes back, playing nothing.
func TestTheConsoleChoosesAnotherDACAndBack(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	cs := bootConsoleServe(t, playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn()})
	eng := cs.runningEngine(t)
	availableOutput(t, eng)
	queue(t, eng, 0, someReference(t, "0123456789abcdef"))
	if err := eng.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitEngine(t, eng, "playing", func(s *engine.Snapshot) bool { return s.State() == engine.Playing })
	row := cs.dacRow(t)
	if !row.Running || !row.Configured || row.Selection == "" || row.Recommended != config.BindingSerial {
		t.Fatalf("the GO bar's row: %+v, want the DAC the player plays, configured, with a token", row)
	}
	model := cs.change(t, "/api/output/player/dac", `{"selection":"`+row.Selection+`","binding":"uniqueModel"}`, "restart")
	if !model.Interrupted || model.Player.Running.Withheld != reasonBindingChanged || strings.Join(pendingChanges(model.Player), ",") != "dac" {
		t.Errorf("another binding while playing: interrupted %v, withheld %q, pending %v", model.Interrupted, model.Player.Running.Withheld, pendingChanges(model.Player))
	}
	awaitEngine(t, eng, "binding_changed and playback stopped", func(s *engine.Snapshot) bool {
		return s.Output().Reason == reasonBindingChanged && s.State() == engine.Stopped
	})
	if d := cs.onDisk(t).Device; d.Binding != config.BindingUniqueModel || d.Serial.Present() || d.VendorID != "20b1" {
		t.Fatalf("the file's binding: %+v, want the GO bar by its model", d)
	}
	back := cs.change(t, "/api/output/player/dac", `{"selection":"`+cs.dacRow(t).Selection+`","binding":"serial"}`, "restart")
	if back.Interrupted || back.Player.Running.Withheld != "" || len(back.Player.Pending) != 0 {
		t.Errorf("the boot binding chosen again: interrupted %v, withheld %q, pending %v; want nothing pending", back.Interrupted, back.Player.Running.Withheld, pendingChanges(back.Player))
	}
	s := awaitEngine(t, eng, "the output back", func(s *engine.Snapshot) bool { return s.Output().Available })
	if s.State() != engine.Stopped {
		t.Errorf("the output's return played: %s", s.State())
	}
	if d := cs.onDisk(t).Device; d.Binding != config.BindingSerial || !d.Serial.Equal(usbid.New(goBarSerial)) || d.Transport != "" {
		t.Errorf("the file's binding: %+v, want the boot binding as written, by serial, no transport", d)
	}
	cs.change(t, "/api/output/player/dac", `{"selection":"`+cs.dacRow(t).Selection+`","binding":"serial"}`, "unchanged")
	if code := cs.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
	}
}

// TestTheConsoleTurnsAPlayerOnForTheNextStart: a player off at boot, turned on
// from the console, starts nothing now and says it starts at the next start;
// the next serve of the file the console saved starts it.
func TestTheConsoleTurnsAPlayerOnForTheNextStart(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	cs := bootConsoleServe(t, playerServeCase{player: goBarOff, standIn: newPlayerStandIn()})
	if eng := cs.engine(t); eng != nil {
		t.Fatal("a player off at boot started")
	}
	st := cs.status(t)
	if st.Running.Started || st.Running.State != "off" || st.Configured.Enabled || st.Configured.Starts || len(st.Pending) != 0 {
		t.Fatalf("a player off at boot: running %+v, configured %+v, pending %v", st.Running, st.Configured, st.Pending)
	}
	on := cs.change(t, "/api/output/player/enabled", `{"enabled":true}`, "restart")
	if on.Interrupted || !on.Player.Configured.Starts || strings.Join(pendingChanges(on.Player), ",") != "start" || on.Player.Running.Started {
		t.Fatalf("turned on: %+v", on)
	}
	if cs.player.eng != nil {
		t.Fatal("the console started the player")
	}
	if code := cs.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
	}
	next := bootPlayerServeAt(t, playerServeCase{keep: true, standIn: newPlayerStandIn()}, filepath.Dir(cs.configPath), cs.music, cs.dataDir)
	next.runningEngine(t)
	if code := next.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, next.stderr.String())
	}
}

// TestARestartFromThePageWhilePlayingComesBackStopped: the page's restart is
// the console's own (POST /api/restart). Asked while the player plays, it
// stops the player with the rest of serve, which exits to be started again
// (supervision.RestartExitCode); the next serve of that data directory starts
// the player with its queue, stopped, playing nothing once its output is
// back, as the page's note says.
func TestARestartFromThePageWhilePlayingComesBackStopped(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	cs := bootConsoleServe(t, playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn()})
	eng := cs.runningEngine(t)
	availableOutput(t, eng)
	id := queue(t, eng, 0, someReference(t, "0123456789abcdef"))
	if err := eng.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitEngine(t, eng, "playing", func(s *engine.Snapshot) bool { return s.State() == engine.Playing })
	if code, body := cs.call(t, http.MethodPost, "/api/restart", "", nil); code != http.StatusAccepted || !strings.Contains(body, `"restarting":true`) {
		t.Fatalf("POST /api/restart while playing = %d: %s; want 202 restarting", code, body)
	}
	select {
	case code := <-cs.done:
		if code != supervision.RestartExitCode {
			t.Fatalf("serve exited %d after the page's restart, want %d; stderr=%s", code, supervision.RestartExitCode, cs.stderr.String())
		}
	case <-serveGiveUp(t):
		t.Fatalf("serve did not return after the page's restart; stderr=%s\nserve's goroutines:\n%s", cs.stderr.String(), serveStacks())
	}
	next := bootPlayerServeAt(t, playerServeCase{keep: true, standIn: newPlayerStandIn()}, filepath.Dir(cs.configPath), cs.music, cs.dataDir)
	eng = next.runningEngine(t)
	availableOutput(t, eng)
	if s := eng.Snapshot(); s.State() != engine.Stopped || s.Len() != 1 || s.IDs()[0] != id {
		t.Errorf("the player after the restart: %s with %v queued, want stopped with entry %d", s.State(), s.IDs(), id)
	}
	if code := next.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, next.stderr.String())
	}
}

// TestTheConsoleOffersNoPlayerOffLinux: on a host that is not Linux the page's
// status says why, and its routes refuse, reading no box and writing nothing.
func TestTheConsoleOffersNoPlayerOffLinux(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	cs := bootConsoleServe(t, playerServeCase{player: goBarOff, goos: "darwin"})
	if st := cs.status(t); st.Available || st.Unavailable != "player_not_supported" {
		t.Fatalf("off Linux: available %v, unavailable %q", st.Available, st.Unavailable)
	}
	before := readBytes(t, cs.configPath)
	for _, r := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/output/player/dacs", ""},
		{http.MethodPost, "/api/output/player/enabled", `{"enabled":true}`},
		{http.MethodPost, "/api/output/player/dac", `{"selection":"x","binding":"serial"}`},
	} {
		if code, body := cs.call(t, r.method, r.path, r.body, nil); code != http.StatusConflict || !strings.Contains(body, "player_not_supported") {
			t.Errorf("%s %s off Linux = %d: %s", r.method, r.path, code, body)
		}
	}
	if after := readBytes(t, cs.configPath); string(after) != string(before) {
		t.Errorf("a refused route wrote the config")
	}
	if code := cs.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
	}
}
