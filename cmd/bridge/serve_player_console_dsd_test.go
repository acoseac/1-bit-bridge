package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// The Player page's transports table (B1029): each DSD row's Set to is the
// choice GET /v1/outputs publishes for the bound DAC, and its rates are the
// set the player's backend publishes, the rates it converts to PCM on a row of
// their own with their PCM plan; never the boot snapshot's legacy DSD keys.

// consoleRow is a transport's row of the Player page's status, decoded from
// its JSON as the page reads it.
type consoleRow struct {
	Kind    string   `json:"kind"`
	Plays   bool     `json:"plays"`
	Capable string   `json:"capable"`
	Mode    string   `json:"mode"`
	Rates   []string `json:"rates"`
	Note    string   `json:"note"`
}

// consoleRows are rep's transport rows by kind, failing the test, and going
// on, unless they are pcm, dop, native and converted, in that order.
func consoleRows(t *testing.T, rep admin.PlayerReport) map[string]consoleRow {
	t.Helper()
	raw, err := json.Marshal(rep.Transports)
	if err != nil {
		t.Fatal(err)
	}
	var rows []consoleRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	out := map[string]consoleRow{}
	var kinds []string
	for _, r := range rows {
		kinds = append(kinds, r.Kind)
		out[r.Kind] = r
	}
	if strings.Join(kinds, " ") != "pcm dop native converted" {
		t.Errorf("the transport rows are %v, want pcm, dop, native and converted", kinds)
	}
	return out
}

// dsdConsole is the console's view of a player whose real engine runs over a
// backend (the choice rig's: the output available, the transport stopped),
// beside GET /v1/outputs and its PUT, served over the view the console reads,
// and boot, the boot snapshot of its config.
type dsdConsole struct {
	*choiceRig
	c    *consolePlayer
	boot *config.Config
}

// newDSDConsole is a dsdConsole over be, its boot config the GO bar's changed
// by boot (nil for none: no legacy DSD key).
func newDSDConsole(t *testing.T, be *playback.Backend, boot func(*config.PlayerConfig)) *dsdConsole {
	t.Helper()
	r := newChoiceRig(t, be)
	cfg := playerOutputConfig(r.view.dataDir, boot)
	c := newConsolePlayer(consolePlayerWiring{Plan: r.view.plan, Player: r.view.p, View: r.view, Boot: cfg, GOOS: "linux", LockHeld: true})
	return &dsdConsole{choiceRig: r, c: c, boot: cfg}
}

// rows is the page's status read now on live, the boot config for nil.
func (d *dsdConsole) rows(t *testing.T, live *config.Config) map[string]consoleRow {
	t.Helper()
	if live == nil {
		live = d.boot
	}
	return consoleRows(t, d.c.Report(context.Background(), live))
}

// outputsChoice is GET /v1/outputs' dsdMode and allowDoP.
func (d *dsdConsole) outputsChoice(t *testing.T) (string, bool) {
	t.Helper()
	obj := d.bridge(t)
	var mode string
	var allow bool
	if err := json.Unmarshal(obj["dsdMode"], &mode); err != nil {
		t.Fatalf("dsdMode %s: %v", obj["dsdMode"], err)
	}
	if err := json.Unmarshal(obj["allowDoP"], &allow); err != nil {
		t.Fatalf("allowDoP %s: %v", obj["allowDoP"], err)
	}
	return mode, allow
}

// requireSetTo requires the DSD rows' Set to: choice on the native and
// converted rows, and on the DoP row with dop after it.
func requireSetTo(t *testing.T, what string, rows map[string]consoleRow, choice, dop string) {
	t.Helper()
	for kind, want := range map[string]string{"dop": choice + "; " + dop, "native": choice, "converted": choice} {
		if got := rows[kind].Mode; got != want {
			t.Errorf("%s: the %s row is set to %q, want %q", what, kind, got, want)
		}
	}
}

// ratesOf is where each DSD rate shows in rows: the kind of the row whose
// rates name it, by the rate's name, each rate on one row at most.
func ratesOf(t *testing.T, rows map[string]consoleRow) map[string]string {
	t.Helper()
	out := map[string]string{}
	for kind, r := range rows {
		for _, text := range r.Rates {
			for _, name := range []string{"DSD64", "DSD128", "DSD256"} {
				if !strings.HasPrefix(text, name+" ") {
					continue
				}
				if was, ok := out[name]; ok {
					t.Errorf("%s is on the %s row and the %s row", name, was, kind)
				}
				out[name] = kind
			}
		}
	}
	return out
}

// convertedAt176 are the converted row's rates for rates, each to the plan a
// DAC that takes 176.4 kHz at 24 bits is converted to.
func convertedAt176(names ...string) []string {
	var out []string
	for _, n := range names {
		out = append(out, n+" to 176.4 kHz 24-bit PCM")
	}
	return out
}

// TestAFreshStateSaysItsChoiceNotTheBootKeys: on a fresh install the DSD
// state holds the bridge's default, auto, for a DAC with no choice of its
// own, and the boot snapshot sets no legacy DSD key; the DoP, native and
// converted rows are set to that choice as GET /v1/outputs publishes it,
// never to the boot keys' off.
func TestAFreshStateSaysItsChoiceNotTheBootKeys(t *testing.T) {
	d := newDSDConsole(t, readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(dsdstate.Defaults())), nil)
	if keys := d.boot.Output.Player.LegacyDSDKeys(); len(keys) != 0 {
		t.Fatalf("the boot snapshot sets %v: this case is a fresh install's, with none", keys)
	}
	if mode, allow := d.outputsChoice(t); mode != dsdstate.ModeAuto || allow {
		t.Fatalf("GET /v1/outputs says dsdMode %q, allowDoP %v: this case is the bridge's default auto on a DAC no profile knows", mode, allow)
	}
	rows := d.rows(t, nil)
	requireSetTo(t, "a fresh state", rows, "auto, the bridge's default", "DoP not allowed")
	if pcm := rows["pcm"]; pcm.Mode != "always on: no DSD choice applies to it" || !slices.Equal(pcm.Rates, []string{"every rate the DAC takes"}) {
		t.Errorf("the PCM row: set to %q, rates %q", pcm.Mode, pcm.Rates)
	}
}

// TestADACThatPlaysDSDOnlyConvertedSaysSo: a DAC no profile knows, under the
// bridge's auto, plays every DSD rate converted to PCM (176.4 kHz at 24 bits,
// what its altsets take): the converted row plays, the DAC takes it, and it
// lists each rate with its PCM plan; the DoP and native rows play nothing and
// list no rate, and the PCM row's bit-exact claim names no converted rate.
func TestADACThatPlaysDSDOnlyConvertedSaysSo(t *testing.T) {
	d := newDSDConsole(t, readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(dsdstate.Defaults())), nil)
	rows := d.rows(t, nil)
	conv := rows["converted"]
	if !conv.Plays || conv.Capable != "yes" || !slices.Equal(conv.Rates, convertedAt176("DSD64", "DSD128", "DSD256")) {
		t.Errorf("the converted row: plays %v, capable %q, rates %q; want it playing, the DAC taking it, every rate with its plan", conv.Plays, conv.Capable, conv.Rates)
	}
	if !strings.Contains(conv.Note, "not bit-exact") {
		t.Errorf("the converted row's note %q does not say what it sends is not bit-exact", conv.Note)
	}
	for _, kind := range []string{"dop", "native"} {
		if r := rows[kind]; r.Plays || len(r.Rates) != 0 {
			t.Errorf("the %s row plays %v at %q: no DSD plays %s on this DAC", kind, r.Plays, r.Rates, kind)
		}
	}
	pcm := rows["pcm"]
	if !pcm.Plays || strings.Contains(pcm.Note+strings.Join(pcm.Rates, " "), "DSD") {
		t.Errorf("the PCM row: plays %v, rates %q, note %q; want it playing, naming no DSD rate", pcm.Plays, pcm.Rates, pcm.Note)
	}
	if got := ratesOf(t, rows); !maps.Equal(got, map[string]string{"DSD64": "converted", "DSD128": "converted", "DSD256": "converted"}) {
		t.Errorf("each rate's row: %v, want every rate on the converted row", got)
	}
}

// TestACappedChoiceGivesEachRateItsRow: the DAC's own choice of auto capped
// at DSD64 leaves DSD64 to native DSD (a DAC whose profile and altsets take
// it) or to DoP (its altsets taking no native DSD, its profile having heard
// DoP), and converts DSD128 and DSD256 to 176.4 kHz at 24 bits: each rate is
// on its row alone, the converted ones with their plan; the rows are set to
// the capped choice. Then, with those facts still published, the output
// closed, by the live config (the player turned off) and by the DAC gone:
// every row's Plays is false, and the rows still name their rates.
func TestACappedChoiceGivesEachRateItsRow(t *testing.T) {
	capped := func() dsdstate.State {
		st := playbacktest.ChoiceState(dsdstate.ModeAuto)
		st.DACs[0].Choice.MaxBitRate = playbacktest.DSD64
		return st
	}
	for _, tc := range []struct {
		name, kind, tuple string
		box               func() *playbacktest.Box
	}{
		{"native at DSD64", "native", "DSD64 DSD_U32_BE", func() *playbacktest.Box { return playbacktest.NewBox(5) }},
		{"DoP at DSD64", "dop", "DSD64 S32_LE", func() *playbacktest.Box {
			box := playbacktest.NewBox(5)
			box.SetAltsets(5, 0, playbacktest.S32Altset(), playbacktest.S24Altset())
			return box
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDSDConsole(t, readSideBackend(t, tc.box(), playbacktest.NewMemStore(capped()), withKnownDAC), nil)
			want := map[string]consoleRow{
				tc.kind:     {Plays: true, Rates: []string{tc.tuple}},
				"converted": {Plays: true, Rates: convertedAt176("DSD128", "DSD256")},
			}
			check := func(what string, rows map[string]consoleRow, open bool) {
				t.Helper()
				for _, kind := range []string{"pcm", "dop", "native", "converted"} {
					r, w := rows[kind], want[kind]
					if kind == "pcm" {
						w = consoleRow{Plays: true, Rates: []string{"every rate the DAC takes"}}
					}
					if r.Plays != (w.Plays && open) || !slices.Equal(r.Rates, w.Rates) {
						t.Errorf("%s: the %s row plays %v at %q, want %v at %q", what, kind, r.Plays, r.Rates, w.Plays && open, w.Rates)
					}
				}
				wantRates := map[string]string{"DSD64": tc.kind, "DSD128": "converted", "DSD256": "converted"}
				if got := ratesOf(t, rows); !maps.Equal(got, wantRates) {
					t.Errorf("%s: each rate's row %v, want %v", what, got, wantRates)
				}
			}
			rows := d.rows(t, nil)
			check("the output open", rows, true)
			requireSetTo(t, "a capped choice", rows, "auto up to DSD64, this DAC's own choice", "DoP allowed")

			off := playerOutputConfig(d.boot.DataDir, func(p *config.PlayerConfig) { p.Enabled = false })
			rows = d.rows(t, off)
			check("the player turned off", rows, false)
			for kind, r := range rows {
				if !strings.HasPrefix(r.Note, "The player's output is not available now (disabled), so nothing plays.") {
					t.Errorf("the player turned off: the %s row's note %q does not say the output is withheld", kind, r.Note)
				}
			}

			if err := d.eng.ReportOutput(context.Background(), engine.OutputReport{Generation: 2, Reason: string(binding.StateDeviceMissing)}); err != nil {
				t.Fatal(err)
			}
			awaitEngine(t, d.eng, "the output gone", func(s *engine.Snapshot) bool { return s.Output().Known && !s.Output().Available })
			rows = d.rows(t, nil)
			check("the DAC gone", rows, false)
			for kind, r := range rows {
				if !strings.HasPrefix(r.Note, "The player's output is not available now (device_missing), so nothing plays.") {
					t.Errorf("the DAC gone: the %s row's note %q does not say the output is gone", kind, r.Note)
				}
			}
		})
	}
}

// TestAChoiceWrittenThroughTheRouteShowsAtTheNextRead: a DAC whose profile
// takes native DSD plays every rate natively under the bridge's auto; the
// app's per-DAC route writes its choice, convert, and at the console's next
// read every rate is on the converted row and the rows are set to the DAC's
// own convert, as GET /v1/outputs then says, while the boot snapshot keeps
// the legacy keys it held (dsdMode off, allowDoP false), which the table
// never reads.
func TestAChoiceWrittenThroughTheRouteShowsAtTheNextRead(t *testing.T) {
	no := false
	d := newDSDConsole(t, readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(dsdstate.Defaults()), withKnownDAC),
		func(p *config.PlayerConfig) { p.DSDMode, p.AllowDoP = dsdstate.ModeOff, &no })
	bootKeys := func() string {
		p := d.boot.Output.Player
		return p.EffectiveDSDMode() + " " + strings.Join(p.LegacyDSDKeys(), ",")
	}
	keys := bootKeys()
	before := d.rows(t, nil)
	requireSetTo(t, "before the write", before, "auto, the bridge's default", "DoP allowed")
	if n := before["native"]; !n.Plays || len(n.Rates) != 3 || before["converted"].Plays {
		t.Errorf("before the write: native plays %v at %q, converted plays %v; want every rate native", n.Plays, n.Rates, before["converted"].Plays)
	}
	wantAnswer(t, "the app's write of convert", d.put(t, choiceBody(t, d.dac(t), dsdstate.ModeConvert)), http.StatusOK, "", 4)
	after := d.rows(t, nil)
	requireSetTo(t, "after the write", after, "convert, this DAC's own choice", "DoP allowed")
	if c, n := after["converted"], after["native"]; !c.Plays || !slices.Equal(c.Rates, convertedAt176("DSD64", "DSD128", "DSD256")) || n.Plays || len(n.Rates) != 0 {
		t.Errorf("after the write: converted plays %v at %q, native plays %v at %q; want every rate converted", c.Plays, c.Rates, n.Plays, n.Rates)
	}
	if mode, allow := d.outputsChoice(t); mode != dsdstate.ModeConvert || !allow {
		t.Errorf("GET /v1/outputs after the write: dsdMode %q, allowDoP %v; want convert, DoP allowed by the profile", mode, allow)
	}
	if now := bootKeys(); now != keys || now != "off dsdMode,allowDoP" {
		t.Errorf("the boot snapshot's legacy keys: %q, then %q; want off, dsdMode and allowDoP set, unchanged", keys, now)
	}
}

// TestTheTransportsSetToIsWhatTheOutputsBodySays: a listed player's rows are
// set to what GET /v1/outputs says, dsdMode and allowDoP, and an unlisted one
// publishes no choice: with no DAC available (off, DoP not allowed, every
// rate refused); on a DAC whose profile heard DoP and whose owner confirmed
// none (DoP allowed, which the choice's dopConfirmed alone would not say);
// and for a player serve did not start, and one starting, which GET
// /v1/outputs does not list.
func TestTheTransportsSetToIsWhatTheOutputsBodySays(t *testing.T) {
	t.Run("no DAC available", func(t *testing.T) {
		d := newDSDConsole(t, readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(dsdstate.Defaults()), func(p *playback.DSDPolicy) {
			p.Bound = func() (playback.BoundDAC, bool) { return playback.BoundDAC{}, false }
		}), nil)
		if mode, allow := d.outputsChoice(t); mode != dsdstate.ModeOff || allow {
			t.Fatalf("GET /v1/outputs says dsdMode %q, allowDoP %v; want off, as it says with no DAC available", mode, allow)
		}
		rows := d.rows(t, nil)
		requireSetTo(t, "no DAC available", rows, "off", "DoP not allowed")
		for _, kind := range []string{"dop", "native", "converted"} {
			if r := rows[kind]; r.Plays || len(r.Rates) != 0 || !strings.HasSuffix(r.Note, "No DSD rate plays now (dsd_dac_unread).") {
				t.Errorf("the %s row with no DAC available: plays %v at %q, note %q", kind, r.Plays, r.Rates, r.Note)
			}
		}
	})

	t.Run("DoP allowed by the profile alone", func(t *testing.T) {
		d := newDSDConsole(t, readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(dsdstate.Defaults()), withKnownDAC), nil)
		if dac := d.dac(t); dac.Choice.DoPConfirmed {
			t.Fatalf("dsdDAC's choice %+v: this case is a DAC whose owner confirmed no DoP", dac.Choice)
		}
		if _, allow := d.outputsChoice(t); !allow {
			t.Fatal("GET /v1/outputs allows no DoP: this case is a profile that heard it")
		}
		requireSetTo(t, "a profile that heard DoP", d.rows(t, nil), "auto, the bridge's default", "DoP allowed")
	})

	unlisted := func(t *testing.T, what string, c *consolePlayer, live *config.Config) {
		t.Helper()
		rows := consoleRows(t, c.Report(context.Background(), live))
		for _, kind := range []string{"dop", "native", "converted"} {
			if r := rows[kind]; r.Mode != "none published" || r.Plays || len(r.Rates) != 0 {
				t.Errorf("%s: the %s row: set to %q, plays %v at %q; want none published, nothing playing", what, kind, r.Mode, r.Plays, r.Rates)
			}
		}
	}
	t.Run("a player serve did not start", func(t *testing.T) {
		live := playerOutputConfig(t.TempDir(), nil)
		unlisted(t, "a player serve did not start", newConsolePlayer(consolePlayerWiring{GOOS: "linux", Boot: live}), live)
	})
	t.Run("a player starting", func(t *testing.T) {
		live := playerOutputConfig(t.TempDir(), nil)
		plan := servePlayerPlan{lists: true, start: true}
		p := &bridgePlayer{ready: make(chan struct{}), device: live.Output.Player.Device}
		c := newConsolePlayer(consolePlayerWiring{Plan: plan, Player: p, View: newBridgeOutput(plan, p, live), Boot: live, GOOS: "linux", LockHeld: true})
		unlisted(t, "a player starting, with no identity yet", c, live)
	})
}
