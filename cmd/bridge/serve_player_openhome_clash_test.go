package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// A clash with the OpenHome surface's port that reaches the config while
// serve runs (B612's second round). What runs is judged by what serve booted
// with: its listeners bound once, and its surface started from the boot
// snapshot's opt-in, so the running player's admission (serve's player, its
// watcher, the console's view of it) judges the clash on the boot snapshot,
// and a live listener address or a live opt-in waits for the next start, as
// the surface does. What comes next is judged by the saved config: the next
// start refuses the player over a clash in its file, and the console refuses
// to save one while the opt-in is on.

// openHomeClash is one setting of serve's listeners whose port
// config.OpenHomePortClash compares with the OpenHome surface's: its name,
// as a refusal names it, and how a config moves it onto a port.
type openHomeClash struct {
	setting string
	move    func(c *config.Config, port int)
}

// openHomeClashes are the four.
var openHomeClashes = []openHomeClash{
	{"listenAddress", func(c *config.Config, port int) { c.ListenAddress = fmt.Sprintf(":%d", port) }},
	{"adminAddress", func(c *config.Config, port int) { c.AdminAddress = fmt.Sprintf("127.0.0.1:%d", port) }},
	{"dlna.listenAddress", func(c *config.Config, port int) { c.DLNA.ListenAddress = fmt.Sprintf(":%d", port) }},
	{"output.loopbackPort", func(c *config.Config, port int) { c.Output.LoopbackPort = &port }},
}

// playing queues an entry on eng, plays it, and waits until the engine plays.
func playing(t *testing.T, eng *engine.Engine) {
	t.Helper()
	availableOutput(t, eng)
	queue(t, eng, 0, someReference(t, "0123456789abcdef"))
	if err := eng.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitEngine(t, eng, "playing", func(s *engine.Snapshot) bool { return s.State() == engine.Playing })
}

// stillPlaying fails the test at once unless the engine plays with its output
// available and the DAC was neither opened nor let go since counts were read:
// what follows would wait out the binary's deadline on a player stopped.
func stillPlaying(t *testing.T, eng *engine.Engine, standIn *playerStandIn, opened, closed, starts int, what string) {
	t.Helper()
	ok := true
	if got := eng.Snapshot(); got.State() != engine.Playing || !got.Output().Available {
		t.Errorf("%s: the engine is %s, its output %+v; want playing, available", what, got.State(), got.Output())
		ok = false
	}
	if o, c, n := standIn.counts(); o != opened || c != closed || n != starts {
		t.Errorf("%s: the DAC was opened %d, let go %d and started %d times; want %d, %d and %d", what, o, c, n, opened, closed, starts)
		ok = false
	}
	if !ok {
		t.FailNow()
	}
}

// TestTheConsoleRefusesToMoveAListenerOntoTheOpenHomePort is the sequence
// Codex found (B612's second round): a bridge booted with the OpenHome
// surface off plays; the console's switch turns the opt-in on, which is
// saved and waits for a restart; a settings save then moves the API's
// address, or the console's (127.0.0.1, which a loopback console's own
// check takes), onto the surface's port. The save is refused, 409 naming the
// setting and never the port, and nothing is written: the file, the live
// config and what the console says waits for a restart are as they were,
// and the engine plays on, the DAC held, with no report withholding its
// output before the barrier. Saved, it turned the running player off at
// once, though neither the listener nor the surface had moved, and the next
// start would have refused the player.
func TestTheConsoleRefusesToMoveAListenerOntoTheOpenHomePort(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	for _, tc := range []struct{ setting, value string }{
		{"listenAddress", ":7792"},
		{"adminAddress", "127.0.0.1:7792"},
	} {
		t.Run(tc.setting, func(t *testing.T) {
			box := newOpenHomeBox(t)
			reports := newReportLog()
			standIn := newPlayerStandIn()
			cs := bootConsoleServe(t, playerServeCase{player: goBarPlayer, standIn: standIn,
				seams: &servePlayerSeams{reports: reports.note},
				with:  func(o *serveOpts) { o.playerOpenHome = box.wiring() }})
			eng := cs.runningEngine(t)
			playing(t, eng)
			if on := cs.change(t, "/api/output/player/openhome", openHomeBody(true), "restart"); pendingOpenHome(on.Player) == "" {
				t.Fatalf("the opt-in turned on waits for nothing: pending %v", pendingChanges(on.Player))
			}
			baseline := reports.count()
			opened, closed, starts := standIn.counts()
			file, held, pending := readBytes(t, cs.configPath), cs.holder.Load(), cs.status(t).Pending

			code, raw := cs.call(t, http.MethodPatch, "/api/settings", fmt.Sprintf(`{%q:%q}`, tc.setting, tc.value), nil)
			if code != http.StatusConflict || !strings.Contains(raw, `"openhome-port-clash"`) || !strings.Contains(raw, tc.setting) ||
				!strings.Contains(raw, "output.player.openhomePort") || strings.Contains(raw, "7792") {
				t.Errorf("PATCH %s onto the OpenHome port = %d: %s; want 409 openhome-port-clash naming the setting, not the port", tc.setting, code, raw)
			}
			if string(readBytes(t, cs.configPath)) != string(file) {
				t.Errorf("a refused save wrote the config file")
			}
			if cs.holder.Load() != held {
				t.Errorf("a refused save changed the live config")
			}
			if got := cs.status(t).Pending; !reflect.DeepEqual(got, pending) {
				t.Errorf("a refused save changed what waits for a restart: %+v, was %+v", got, pending)
			}
			stillPlaying(t, eng, standIn, opened, closed, starts, "after the refused save")
			if surfaceOf(cs.player) != nil || len(box.Listens()) != 0 {
				t.Errorf("a surface started in the running serve: listens %v", box.Listens())
			}
			withholdsOnlyForTheBarrier(t, cs, reports, baseline)
			if code := cs.stopServe(t); code != 0 {
				t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
			}
		})
	}
}

// TestALiveClashWithTheOpenHomePortLeavesTheRunningPlayerAsItIs pins the
// running player's admission against a clash that reaches the live config
// with no console save, as a holder Store lands (memory alone, the file
// untouched): with the surface on at boot, each of the four listeners'
// settings moved onto its port leaves the engine playing, the DAC held and
// the surface listening where the boot put it, since no socket moved; the
// console says the player plays and withholds nothing, and that the next
// start refuses it, naming the setting; a player-off request without the
// operator's go-ahead is still asked first (would_interrupt), since what the
// console compares is what serve's player hears; and the first report that
// withholds the output is the barrier's, a binding changed live, which
// applies at once beside the clash.
func TestALiveClashWithTheOpenHomePortLeavesTheRunningPlayerAsItIs(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	for _, clash := range openHomeClashes {
		t.Run(clash.setting, func(t *testing.T) {
			box := newOpenHomeBox(t)
			reports := newReportLog()
			standIn := newPlayerStandIn()
			cs := bootConsoleServe(t, playerServeCase{player: goBarPlayer + "    openhome: true\n", standIn: standIn,
				seams: &servePlayerSeams{reports: reports.note},
				with:  func(o *serveOpts) { o.playerOpenHome = box.wiring() }})
			eng := cs.runningEngine(t)
			s, at := box.surface(t)
			playing(t, eng)
			baseline := reports.count()
			opened, closed, starts := standIn.counts()

			edited := config.Clone(cs.holder.Load())
			clash.move(edited, config.DefaultOpenHomePort)
			if err := edited.OpenHomePortClash(); err == nil || !strings.Contains(err.Error(), "for "+clash.setting+":") {
				t.Fatalf("the edit makes no clash through %s: %v", clash.setting, err)
			}
			cs.holder.Store(edited)

			st := cs.status(t)
			if st.Running.State != bridgeStatePlaying || st.Running.Withheld != "" {
				t.Errorf("after the edit the console says the player is %s, withheld %q; want playing, nothing withheld", st.Running.State, st.Running.Withheld)
			}
			if st.Configured.Starts || !strings.Contains(st.Configured.Reason, "for "+clash.setting+":") || strings.Contains(st.Configured.Reason, "7792") {
				t.Errorf("after the edit the next start: starts %v, %q; want it refused, naming %s and not the port", st.Configured.Starts, st.Configured.Reason, clash.setting)
			}
			if len(st.Pending) != 1 || st.Pending[0].Change != "stop" || st.Pending[0].Text != "The player stops when the bridge restarts." {
				t.Errorf("after the edit: pending %+v; want the player stopping at the restart, and nothing about now", st.Pending)
			}
			cs.askedFirst(t, "/api/output/player/enabled", `{"enabled":false}`, eng)
			stillPlaying(t, eng, standIn, opened, closed, starts, "after the edit")
			listensAt(t, s, at, "after the edit")
			withholdsOnlyForTheBarrier(t, cs, reports, baseline)
			if code := cs.stopServe(t); code != 0 {
				t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
			}
		})
	}
}

// TestTheRunningPlayerStillFollowsTheLiveConfigBesideAClash: with the surface
// on at boot and a clash with its port in the live config, what the live
// config says of the player itself still reaches it at once, as it does with
// no clash (B601): the console's switch turning it off, with the operator's
// go-ahead, withholds the output as disabled and is answered interrupted,
// and a live player block the config refuses for a reason of its own (a room
// past its 64 characters) turns it off as disabled, naming that reason.
func TestTheRunningPlayerStillFollowsTheLiveConfigBesideAClash(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, cs *consoleServe, edited *config.Config)
		off    string
	}{
		{"the console's switch turns it off", func(t *testing.T, cs *consoleServe, edited *config.Config) {
			cs.holder.Store(edited)
			if ch := cs.change(t, "/api/output/player/enabled", `{"enabled":false,"interrupt":true}`, "restart"); !ch.Interrupted {
				t.Errorf("the switch turning the playing player off beside a clash was not answered interrupted")
			}
		}, ""},
		{"a block the config refuses", func(t *testing.T, cs *consoleServe, edited *config.Config) {
			edited.Output.Player.Room = strings.Repeat("r", 65)
			cs.holder.Store(edited)
		}, "output.player.room"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newOpenHomeBox(t)
			reports := newReportLog()
			cs := bootConsoleServe(t, playerServeCase{player: goBarPlayer + "    openhome: true\n", standIn: newPlayerStandIn(),
				seams: &servePlayerSeams{reports: reports.note},
				with:  func(o *serveOpts) { o.playerOpenHome = box.wiring() }})
			eng := cs.runningEngine(t)
			box.surface(t)
			playing(t, eng)
			baseline := reports.count()
			edited := config.Clone(cs.holder.Load())
			edited.ListenAddress = fmt.Sprintf(":%d", config.DefaultOpenHomePort)
			tc.change(t, cs, edited)
			for _, rp := range reports.awaitAfter(t, baseline, "a report that withholds the output", func(rp engine.OutputReport) bool { return !rp.Available }) {
				if !rp.Available && rp.Reason != "disabled" {
					t.Errorf("the output was withheld as %q; want disabled", rp.Reason)
				}
			}
			awaitEngine(t, eng, "the output withheld", func(s *engine.Snapshot) bool { return !s.Output().Available })
			set := cs.player.live()
			if set.Enabled || tc.off == "" && set.Off != nil || tc.off != "" && (set.Off == nil || !strings.Contains(set.Off.Error(), tc.off)) {
				t.Errorf("the player's live settings: enabled %v, off %v; want it off, for %q", set.Enabled, set.Off, tc.off)
			}
			if st := cs.status(t); st.Running.Withheld != "disabled" {
				t.Errorf("the console says the player is withheld %q; want disabled", st.Running.Withheld)
			}
			if code := cs.stopServe(t); code != 0 {
				t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
			}
		})
	}
}

// heldPort is the port of a listener the test holds.
func heldPort(t *testing.T, l heldLoopback) int {
	t.Helper()
	_, ps, err := net.SplitHostPort(l.addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// TestTheNextStartRefusesThePlayerOverAClashEditedIntoItsFile is the other
// half: what comes next is judged by the file. With the surface on at a port
// of the test's own, each of the four listeners' settings edited onto it in
// the config file beside a running serve changes nothing that serve runs
// (it reads its file once, at its start), and the start after it, reading
// the edit, starts neither the player nor its surface and says why, naming
// the setting and never the port, in the console's status, running and
// configured alike. The next start's listener for an address edited onto
// the port is the one the test holds there, since serve binds the address
// its file names: the fixture writes no address of its own over it.
func TestTheNextStartRefusesThePlayerOverAClashEditedIntoItsFile(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	for _, tc := range []struct {
		setting string
		edit    func(text string, port int) string
		hold    func(c *playerServeCase, l *heldLoopback)
	}{
		{"listenAddress", func(text string, port int) string {
			return fmt.Sprintf("listenAddress: 127.0.0.1:%d\n", port) + text
		}, func(c *playerServeCase, l *heldLoopback) { c.lan = l }},
		{"adminAddress", func(text string, port int) string {
			lines := strings.Split(text, "\n")
			for i, l := range lines {
				if strings.HasPrefix(l, "adminAddress:") {
					lines[i] = fmt.Sprintf("adminAddress: 127.0.0.1:%d", port)
				}
			}
			return strings.Join(lines, "\n")
		}, func(c *playerServeCase, l *heldLoopback) { c.admin = l }},
		{"dlna.listenAddress", func(text string, port int) string {
			return fmt.Sprintf("dlna:\n  listenAddress: \":%d\"\n", port) + text
		}, nil},
		{"output.loopbackPort", func(text string, port int) string {
			return strings.Replace(text, "output:\n  mpd: off\n", fmt.Sprintf("output:\n  mpd: off\n  loopbackPort: %d\n", port), 1)
		}, nil},
	} {
		t.Run(tc.setting, func(t *testing.T) {
			next := holdLoopback(t)
			port := heldPort(t, next)
			box := newOpenHomeBox(t)
			standIn := newPlayerStandIn()
			first := bootConsoleServe(t, playerServeCase{player: goBarPlayer + fmt.Sprintf("    openhome: true\n    openhomePort: %d\n", port),
				standIn: standIn, with: func(o *serveOpts) { o.playerOpenHome = box.wiring() }})
			eng := first.runningEngine(t)
			s, at := box.surface(t)
			playing(t, eng)
			opened, closed, starts := standIn.counts()

			raw := string(readBytes(t, first.configPath))
			edited := tc.edit(raw, port)
			if edited == raw {
				t.Fatalf("the edit changed nothing of the file:\n%s", raw)
			}
			if err := os.WriteFile(first.configPath, []byte(edited), 0o600); err != nil {
				t.Fatal(err)
			}
			stillPlaying(t, eng, standIn, opened, closed, starts, "after the file was edited")
			listensAt(t, s, at, "after the file was edited")
			if st := first.status(t); st.Running.Withheld != "" || !st.Configured.Starts {
				t.Errorf("the running serve read its edited file: withheld %q, the next start %v", st.Running.Withheld, st.Configured.Starts)
			}
			if code := first.stopServe(t); code != 0 {
				t.Fatalf("exit %d; stderr=%s", code, first.stderr.String())
			}

			box = newOpenHomeBox(t)
			c := playerServeCase{keep: true, standIn: newPlayerStandIn(), with: func(o *serveOpts) { o.playerOpenHome = box.wiring() }}
			if tc.hold != nil {
				tc.hold(&c, &next)
			}
			second := bootConsoleServeAt(t, c, first)
			if second.player.plan.start || second.engine(t) != nil {
				t.Fatalf("the start after the edit started the player: plan %+v", second.player.plan)
			}
			named := func(text string) bool {
				return strings.Contains(text, "for "+tc.setting+":") && strings.Contains(text, "output.player.openhomePort") &&
					!strings.Contains(text, strconv.Itoa(port))
			}
			if !named(second.player.plan.off) {
				t.Errorf("the start after the edit refused the player for %q; want the clash, naming %s and not the port", second.player.plan.off, tc.setting)
			}
			if surfaceOf(second.player) != nil || len(box.Listens()) != 0 {
				t.Errorf("the start after the edit started a surface: listens %v", box.Listens())
			}
			st := second.status(t)
			if st.Running.Started || st.Running.State != "off" || !named(st.Running.Detail) {
				t.Errorf("the console after the edit: started %v, %s, %q; want the player off, saying why", st.Running.Started, st.Running.State, st.Running.Detail)
			}
			if st.Configured.Starts || !named(st.Configured.Reason) {
				t.Errorf("the console's next start after the edit: starts %v, %q; want it refused, saying why", st.Configured.Starts, st.Configured.Reason)
			}
			if code := second.stopServe(t); code != 0 {
				t.Fatalf("exit %d; stderr=%s", code, second.stderr.String())
			}
		})
	}
}
