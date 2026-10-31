package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
	"github.com/acoseac/1-bit-bridge/internal/player/openhome"
)

// The console's OpenHome switch (B612) against a real serve: what the
// running process does with a change of output.player.openhome, and what the
// next start reads of it. Serve decides its surface once, from the boot
// snapshot (planServePlayer), and its player and its watcher read the live
// opt-in only through config.PlayerEnabled's port check (livePlayerSettings,
// hotplug.SettingsFrom), so a saved change, which the switch refuses over a
// clash, reaches none of them.

// reportLog keeps every output report serve's player hands its engine
// (servePlayerSeams.reports), in order, and wakes a waiter at each.
type reportLog struct {
	mu      sync.Mutex
	reports []engine.OutputReport
	changed chan struct{}
}

func newReportLog() *reportLog { return &reportLog{changed: make(chan struct{})} }

// note records rp; it runs on the player's supervisor, before the engine
// takes the report.
func (l *reportLog) note(rp engine.OutputReport) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reports = append(l.reports, rp)
	close(l.changed)
	l.changed = make(chan struct{})
}

// count is how many reports the player has handed its engine.
func (l *reportLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.reports)
}

// awaitAfter waits, until the test's deadline, for a report after the first
// n that cond holds of, and returns every report after the first n.
func (l *reportLog) awaitAfter(t *testing.T, n int, what string, cond func(engine.OutputReport) bool) []engine.OutputReport {
	t.Helper()
	for {
		l.mu.Lock()
		after, ch := append([]engine.OutputReport(nil), l.reports[min(n, len(l.reports)):]...), l.changed
		l.mu.Unlock()
		if slices.ContainsFunc(after, cond) {
			return after
		}
		select {
		case <-ch:
		case <-serveGiveUp(t):
			t.Fatalf("waiting for the player's output reports: %s; after the first %d: %+v", what, n, after)
		}
	}
}

// withholdsOnlyForTheBarrier changes the live binding to another one of the
// GO bar's (goBarByModel), which withholds the running player's output as
// binding_changed (playerOutput.withheld), waits for that report, and fails
// the test unless every report the player handed its engine after the first
// n withholds for that reason alone. Serve's player reads the live config at
// each of its changes, in order, and the latest whenever it wakes, so a
// change before the barrier that withheld the output (the player turned off
// by a port clash: disabled, which outranks binding_changed) is a report
// before the barrier's, or the barrier's own reason.
func withholdsOnlyForTheBarrier(t *testing.T, cs *consoleServe, reports *reportLog, n int) {
	t.Helper()
	cs.livePlayer(t, func(p *config.PlayerConfig) { p.Device = goBarByModel })
	for _, rp := range reports.awaitAfter(t, n, "the barrier's binding_changed", func(rp engine.OutputReport) bool {
		return !rp.Available && rp.Reason == reasonBindingChanged
	}) {
		if !rp.Available && rp.Reason != reasonBindingChanged {
			t.Errorf("the output was withheld before the barrier: %+v", rp)
		}
	}
}

// openHomeBody is the OpenHome switch's body for on.
func openHomeBody(on bool) string { return fmt.Sprintf(`{"openhome":%v}`, on) }

// pendingOpenHome is the status's pending openHome entry's text, "" for none.
func pendingOpenHome(st admin.PlayerStatus) string {
	for _, p := range st.Pending {
		if p.Change == "openHome" {
			return p.Text
		}
	}
	return ""
}

// sameAsBoot fails the test unless the live config's player settings, as
// serve's player and its watcher read them (livePlayerSettings), are a player
// that runs, with no reason it is off, bound to the GO bar as goBarPlayer
// binds it: the inputs of everything that follows the live config.
func sameAsBoot(t *testing.T, cs *consoleServe, what string) {
	t.Helper()
	set := hotplug.SettingsFrom(cs.holder.Load(), "linux")
	if !set.Enabled || set.Off != nil || set.Device.Binding != config.BindingSerial || set.Device.VendorID != "20b1" {
		t.Errorf("%s: the live player settings are enabled %v, off %v, binding %s", what, set.Enabled, set.Off, set.Device.Binding)
	}
}

// surfaceOf is serve's player's OpenHome surface, nil for none, read under
// the player's mu as GET /v1/outputs' view reads it.
func surfaceOf(p *bridgePlayer) *openhome.Surface {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.surface
}

// listensAt fails the test unless s serves now, on exactly at.
func listensAt(t *testing.T, s *openhome.Surface, at netip.AddrPort, what string) {
	t.Helper()
	st := s.Status()
	if st.Stopped || len(st.Listening) != 1 || st.Listening[0].Bound != at {
		t.Errorf("%s: the surface is %+v, want it listening on %s alone", what, st, at)
	}
}

// TestTheConsolesOpenHomeSwitchLeavesWhatPlaysAsItIs pins what the running
// process does with the console's OpenHome switch (B612): nothing until the
// restart. Turned on while the player plays, and back off while it is paused
// (or off, then on again, with the surface on at boot), each change is saved
// and answered restart, never interrupted; the engine plays on, then holds
// its pause; the DAC is neither opened nor let go again; the surface the
// boot started listens where it listened, and none starts for a boot without
// it; the live settings serve's player and its watcher read are as at boot;
// the pending list carries the change while the opt-in differs from the boot
// snapshot's and clears when it is put back; and no report withholds the
// output before the barrier (withholdsOnlyForTheBarrier).
func TestTheConsolesOpenHomeSwitchLeavesWhatPlaysAsItIs(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	for _, atBoot := range []bool{false, true} {
		t.Run(fmt.Sprintf("the surface on at boot %v", atBoot), func(t *testing.T) {
			box := newOpenHomeBox(t)
			reports := newReportLog()
			standIn := newPlayerStandIn()
			block := goBarPlayer
			if atBoot {
				block += "    openhome: true\n"
			}
			cs := bootConsoleServe(t, playerServeCase{player: block, standIn: standIn,
				seams: &servePlayerSeams{reports: reports.note},
				with:  func(o *serveOpts) { o.playerOpenHome = box.wiring() }})
			eng := cs.runningEngine(t)
			var s *openhome.Surface
			var at netip.AddrPort
			if atBoot {
				s, at = box.surface(t)
			}
			availableOutput(t, eng)
			queue(t, eng, 0, someReference(t, "0123456789abcdef"))
			if err := eng.Play(context.Background()); err != nil {
				t.Fatal(err)
			}
			awaitEngine(t, eng, "playing", func(s *engine.Snapshot) bool { return s.State() == engine.Playing })
			baseline := reports.count()
			opened, closed, starts := standIn.counts()
			unchanged := func(what string, state engine.State) {
				t.Helper()
				if got := eng.Snapshot(); got.State() != state || !got.Output().Available {
					t.Errorf("%s: the engine is %s, its output %+v; want %s, available", what, got.State(), got.Output(), state)
				}
				if o, c, n := standIn.counts(); o != opened || c != closed || n != starts {
					t.Errorf("%s: the DAC was opened %d, let go %d and started %d times; want %d, %d and %d", what, o, c, n, opened, closed, starts)
				}
				sameAsBoot(t, cs, what)
				if atBoot {
					listensAt(t, s, at, what)
				} else if surfaceOf(cs.player) != nil || len(box.Listens()) != 0 {
					t.Errorf("%s: a surface started for a boot without the opt-in: listens %v", what, box.Listens())
				}
			}

			flip := !atBoot
			ch := cs.change(t, "/api/output/player/openhome", openHomeBody(flip), "restart")
			if ch.Interrupted || ch.Player.Running.Withheld != "" || ch.Player.Configured.OpenHome != flip || ch.Player.Running.OpenHome != atBoot {
				t.Errorf("the opt-in turned %v while playing: interrupted %v, withheld %q, configured %v, running %v",
					flip, ch.Interrupted, ch.Player.Running.Withheld, ch.Player.Configured.OpenHome, ch.Player.Running.OpenHome)
			}
			if got := strings.Join(pendingChanges(ch.Player), ","); got != "openHome" {
				t.Errorf("the opt-in changed since boot: pending %s, want openHome", got)
			}
			if text := pendingOpenHome(ch.Player); atBoot && !strings.Contains(text, "Until then the 1-bit app can still cast to it.") ||
				!atBoot && !strings.Contains(text, "as "+openhome.DefaultName+" when it restarts.") {
				t.Errorf("the pending opt-in says %q", text)
			}
			if cs.onDisk(t).OpenHome != flip || cs.holder.Load().Output.Player.OpenHome != flip {
				t.Errorf("the opt-in turned %v is not in the file and the live config", flip)
			}
			unchanged("the opt-in changed while playing", engine.Playing)

			if err := eng.Pause(context.Background()); err != nil {
				t.Fatal(err)
			}
			awaitEngine(t, eng, "paused", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
			back := cs.change(t, "/api/output/player/openhome", openHomeBody(atBoot), "restart")
			if back.Interrupted || len(back.Player.Pending) != 0 || back.Player.Configured.OpenHome != atBoot {
				t.Errorf("the opt-in put back while paused: interrupted %v, pending %v, configured %v; want nothing pending",
					back.Interrupted, pendingChanges(back.Player), back.Player.Configured.OpenHome)
			}
			unchanged("the opt-in put back while paused", engine.Paused)

			withholdsOnlyForTheBarrier(t, cs, reports, baseline)
			if code := cs.stopServe(t); code != 0 {
				t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
			}
		})
	}
}

// TestTheConsolesOpenHomeSwitchRefusesAClashAndChangesNothing: with the
// player playing and openhomePort on the DLNA listener's default port, the
// switch turning the opt-in on is refused (409 player_refused, naming the
// setting and never the port), writes nothing to the file or the live
// config, and changes nothing of what plays: a clash saved would have turned
// the running player off at once (PlayerEnabled, the watcher's and the
// player's live settings), so the refusal is what keeps the switch from
// interrupting anything.
func TestTheConsolesOpenHomeSwitchRefusesAClashAndChangesNothing(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	reports := newReportLog()
	standIn := newPlayerStandIn()
	cs := bootConsoleServe(t, playerServeCase{player: goBarPlayer + "    openhomePort: 7790\n", standIn: standIn,
		seams: &servePlayerSeams{reports: reports.note}})
	eng := cs.runningEngine(t)
	availableOutput(t, eng)
	queue(t, eng, 0, someReference(t, "0123456789abcdef"))
	if err := eng.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitEngine(t, eng, "playing", func(s *engine.Snapshot) bool { return s.State() == engine.Playing })
	baseline := reports.count()
	opened, closed, _ := standIn.counts()
	before := readBytes(t, cs.configPath)
	code, raw := cs.call(t, http.MethodPost, "/api/output/player/openhome", openHomeBody(true), nil)
	if code != http.StatusConflict || !strings.Contains(raw, `"player_refused"`) || !strings.Contains(raw, "dlna.listenAddress") || strings.Contains(raw, "7790") {
		t.Fatalf("the opt-in on over the DLNA listener's port = %d: %s; want 409 player_refused naming the setting, not the port", code, raw)
	}
	if after := readBytes(t, cs.configPath); string(after) != string(before) || cs.holder.Load().Output.Player.OpenHome {
		t.Errorf("a refused opt-in wrote the config")
	}
	if s := eng.Snapshot(); s.State() != engine.Playing || !s.Output().Available {
		t.Errorf("a refused opt-in changed what plays: %s, %+v", s.State(), s.Output())
	}
	if o, c, _ := standIn.counts(); o != opened || c != closed {
		t.Errorf("a refused opt-in opened or let go the DAC")
	}
	sameAsBoot(t, cs, "a refused opt-in")
	if st := cs.status(t); st.Configured.OpenHome || len(st.Pending) != 0 || st.Running.Withheld != "" {
		t.Errorf("after a refused opt-in: configured %+v, pending %v, withheld %q", st.Configured, pendingChanges(st), st.Running.Withheld)
	}
	withholdsOnlyForTheBarrier(t, cs, reports, baseline)
	if code := cs.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
	}
}

// bootConsoleServeAt is bootConsoleServe on directories a first serve laid
// out (bootPlayerServeAt): a next start, on the file its console saved.
func bootConsoleServeAt(t *testing.T, c playerServeCase, first *consoleServe) *consoleServe {
	t.Helper()
	var addr string
	with := c.with
	c.with = func(o *serveOpts) {
		addr = o.adminListener.Addr().String()
		if with != nil {
			with(o)
		}
	}
	ps := bootPlayerServeAt(t, c, filepath.Dir(first.configPath), first.music, first.dataDir)
	return &consoleServe{playerServe: ps, console: addr}
}

// TestTheNextStartReadsTheOpenHomeSwitchTheConsoleSaved: the opt-in the
// console saves is what the next start's plan reads (planServePlayer): turned
// on, the next serve of the file starts the surface with the player, on the
// box's LAN address, and nothing waits for a restart; turned off from that
// serve's console, the one after starts none.
func TestTheNextStartReadsTheOpenHomeSwitchTheConsoleSaved(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	box := newOpenHomeBox(t)
	first := bootConsoleServe(t, playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn(),
		with: func(o *serveOpts) { o.playerOpenHome = box.wiring() }})
	first.runningEngine(t)
	first.change(t, "/api/output/player/openhome", openHomeBody(true), "restart")
	if surfaceOf(first.player) != nil || len(box.Listens()) != 0 {
		t.Fatal("the console's switch started a surface in the running serve")
	}
	if code := first.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, first.stderr.String())
	}

	box = newOpenHomeBox(t)
	second := bootConsoleServeAt(t, playerServeCase{keep: true, standIn: newPlayerStandIn(),
		with: func(o *serveOpts) { o.playerOpenHome = box.wiring() }}, first)
	second.runningEngine(t)
	if !second.player.plan.openhome {
		t.Fatalf("the next start's plan has no surface: %+v", second.player.plan)
	}
	box.surface(t)
	if st := second.status(t); !st.Running.OpenHome || !st.Configured.OpenHome || len(st.Pending) != 0 || st.Running.OpenHomeDetail != "" {
		t.Errorf("the next start: running %v (%q), configured %v, pending %v; want the surface on the LAN, nothing pending",
			st.Running.OpenHome, st.Running.OpenHomeDetail, st.Configured.OpenHome, pendingChanges(st))
	}
	second.change(t, "/api/output/player/openhome", openHomeBody(false), "restart")
	if code := second.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, second.stderr.String())
	}

	box = newOpenHomeBox(t)
	third := bootConsoleServeAt(t, playerServeCase{keep: true, standIn: newPlayerStandIn(),
		with: func(o *serveOpts) { o.playerOpenHome = box.wiring() }}, second)
	third.runningEngine(t)
	if third.player.plan.openhome || surfaceOf(third.player) != nil || len(box.Listens()) != 0 {
		t.Errorf("the start after the opt-in was turned off runs a surface: plan %+v, listens %v", third.player.plan, box.Listens())
	}
	if code := third.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, third.stderr.String())
	}
}

// TestTheConsoleSaysWhatTheOpenHomeSwitchWaitsFor pins the status across the
// states the page words (B612): the opt-in turned on for a player turned off,
// and for one with no DAC bound, waits for a restart that starts the player,
// and nothing of it runs now; a surface the boot started that has no LAN
// address to listen on, or that did not start, says why in the running
// status, and nothing waits for a restart, since a restart cures neither.
func TestTheConsoleSaysWhatTheOpenHomeSwitchWaitsFor(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	const startsThePlayer = "Casting from the 1-bit app is turned on: the bridge puts the player on the LAN as " + openhome.DefaultName + " when a restart starts the player."
	for _, tc := range []struct{ name, block, configured string }{
		{"a player turned off", goBarOff, ""},
		{"a player with no DAC bound", "    enabled: true\n", "no DAC is bound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := bootConsoleServe(t, playerServeCase{player: tc.block, standIn: newPlayerStandIn()})
			if eng := cs.engine(t); eng != nil {
				t.Fatal("the player started")
			}
			on := cs.change(t, "/api/output/player/openhome", openHomeBody(true), "restart").Player
			if !on.Configured.OpenHome || on.Running.OpenHome || on.Running.OpenHomeDetail != "" || pendingOpenHome(on) != startsThePlayer {
				t.Errorf("the opt-in on: configured %v, running %v (%q), pending %q; want it waiting for a restart that starts the player",
					on.Configured.OpenHome, on.Running.OpenHome, on.Running.OpenHomeDetail, pendingOpenHome(on))
			}
			if !strings.Contains(on.Configured.Reason, tc.configured) {
				t.Errorf("the configured player: %q, want it to say %q", on.Configured.Reason, tc.configured)
			}
			if on.Configured.OpenHomeName != openhome.DefaultName || on.Configured.OpenHomePort != config.DefaultOpenHomePort {
				t.Errorf("the configured surface: %q on %d, want %q on %d", on.Configured.OpenHomeName, on.Configured.OpenHomePort, openhome.DefaultName, config.DefaultOpenHomePort)
			}
			if code := cs.stopServe(t); code != 0 {
				t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
			}
		})
	}
	t.Run("a surface with no LAN address", func(t *testing.T) {
		box := newOpenHomeBox(t)
		box.Set([]lan.Address{})
		cs := bootConsoleServe(t, playerServeCase{player: goBarPlayer + "    room: Study\n    openhome: true\n    openhomePort: 0\n", standIn: newPlayerStandIn(),
			with: func(o *serveOpts) { o.playerOpenHome = box.wiring() }})
		cs.runningEngine(t)
		var s *openhome.Surface
		select {
		case s = <-box.started:
		case <-serveGiveUp(t):
			t.Fatal("the player never started its surface")
		}
		for {
			ch := s.Changed()
			if s.Status().Reconciles > 0 {
				break
			}
			select {
			case <-ch:
			case <-serveGiveUp(t):
				t.Fatal("the surface never listed the box")
			}
		}
		st := cs.status(t)
		if st.Running.OpenHome || !strings.HasPrefix(st.Running.OpenHomeDetail, "The box has no LAN address for its OpenHome source yet") {
			t.Errorf("a surface with no address: running %v, %q", st.Running.OpenHome, st.Running.OpenHomeDetail)
		}
		if len(st.Pending) != 0 || !st.Configured.OpenHome || st.Configured.OpenHomeName != "Study" || st.Configured.OpenHomePort != 0 {
			t.Errorf("a surface with no address: pending %v, configured %+v; want nothing pending", pendingChanges(st), st.Configured)
		}
		if code := cs.stopServe(t); code != 0 {
			t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
		}
	})
	t.Run("a surface that did not start", func(t *testing.T) {
		cs := bootConsoleServe(t, playerServeCase{player: goBarPlayer + "    openhome: true\n", standIn: newPlayerStandIn(),
			with: func(o *serveOpts) {
				o.playerOpenHome = openhomeWiring{Start: func(openhome.Config) (*openhome.Surface, error) {
					return nil, errors.New("the surface refused to start")
				}}
			}})
		cs.runningEngine(t)
		st := cs.status(t)
		if st.Running.OpenHome || !strings.HasPrefix(st.Running.OpenHomeDetail, "Its OpenHome source did not start") || len(st.Pending) != 0 {
			t.Errorf("a surface that did not start: running %v, %q, pending %v; want why, and nothing pending",
				st.Running.OpenHome, st.Running.OpenHomeDetail, pendingChanges(st))
		}
		if code := cs.stopServe(t); code != 0 {
			t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
		}
	})
}
