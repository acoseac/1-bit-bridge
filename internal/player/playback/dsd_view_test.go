package playback_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdconv"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// The read side of the player's DSD (B728 B8; the design's section 7.1):
// what the backend publishes of the bound DAC beside the set's tuples, from
// the set it published.

// viewOf is b's published read side, failing the test while there is none.
func viewOf(t *testing.T, b *playback.Backend) (playback.PlayerPolicy, playback.DSDView) {
	t.Helper()
	p := b.Policy()
	if p.DSD == nil {
		t.Fatalf("the set published %+v carries no read side", p)
	}
	return p, *p.DSD
}

// optionOf is v's option for mode.
func optionOf(t *testing.T, v playback.DSDView, mode string) playback.DSDOption {
	t.Helper()
	if v.DAC == nil {
		t.Fatal("the read side names no DAC")
	}
	for _, o := range v.DAC.Options {
		if o.Mode == mode {
			return o
		}
	}
	t.Fatalf("the read side's options %+v hold no %s", v.DAC.Options, mode)
	return playback.DSDOption{}
}

// firstUseOf is v's first use of transport.
func firstUseOf(t *testing.T, v playback.DSDView, transport string) playback.DSDFirstUse {
	t.Helper()
	if v.DAC == nil {
		t.Fatal("the read side names no DAC")
	}
	for _, f := range v.DAC.FirstUse {
		if f.Transport == transport {
			return f
		}
	}
	t.Fatalf("the read side's first uses %+v hold no %s", v.DAC.FirstUse, transport)
	return playback.DSDFirstUse{}
}

// rateOf is the rate bitRate of o, the zero value and false while o offers none.
func rateOf(o playback.DSDOption, bitRate int) (playback.DSDOptionRate, bool) {
	for _, r := range o.Rates {
		if r.BitRate == bitRate {
			return r, true
		}
	}
	return playback.DSDOptionRate{}, false
}

var threeRates = []int{playbacktest.DSD64, playbacktest.DSD128, playbacktest.DSD256}

// TestTheReadSideIsTheSetsOwn: the read side says of the bound DAC what the
// set published was computed from, and nothing else: the choice that
// applies and its revision, what each mode offers at each rate with the
// rung the policy computed, DoP's need of a confirmation, each transport's
// first use, the rates refused with their words and the warnings; each
// tuple's evidence in the tuples' order; and, with no DAC available, no DAC,
// off, and every rate refused for want of a reading.
func TestTheReadSideIsTheSetsOwn(t *testing.T) {
	t.Run("a known DAC whose own choice is native", func(t *testing.T) {
		b := policyBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative)),
			func(p *playback.DSDPolicy) { p.Profiles = playbacktest.KnownProfiles(nil) })
		b.RefreshDSD(true)
		p, v := viewOf(t, b)
		if v.Mode != dsdstate.ModeNative || !v.AllowDoP || len(v.Refused) != 0 || len(v.Warnings) != 0 {
			t.Errorf("mode %q, allowDoP %v, refused %+v, warnings %+v; want native, DoP allowed by its confirmation, nothing refused or to explain", v.Mode, v.AllowDoP, v.Refused, v.Warnings)
		}
		if len(v.Evidence) != len(p.Tuples) || len(p.Tuples) != 3 {
			t.Fatalf("%d tuples beside %d evidences, want three of each", len(p.Tuples), len(v.Evidence))
		}
		for i, e := range v.Evidence {
			if e != (playback.DSDEvidence{Rung: "reported"}) {
				t.Errorf("tuple %d (%+v) carries %+v, want reported by the profile, undated", i, p.Tuples[i].Tuple, e)
			}
		}
		d := v.DAC
		if d == nil {
			t.Fatal("no DAC")
		}
		if d.Key != playbacktest.DACKey().DACKey() {
			t.Errorf("the key %q, want the model key's %q", d.Key, playbacktest.DACKey().DACKey())
		}
		if d.Profile == nil || d.Profile.ID != "test.dac" || d.Profile.Name != "Test DAC" || len(d.Profile.Caveats) != 0 {
			t.Errorf("the profile %+v, want test.dac", d.Profile)
		}
		if d.Choice != (playback.DSDChoice{Mode: dsdstate.ModeNative, Revision: 1, DoPConfirmed: true}) {
			t.Errorf("the choice %+v, want native at revision 1, DoP confirmed", d.Choice)
		}
		native, dop := optionOf(t, v, dsdstate.ModeNative), optionOf(t, v, dsdstate.ModeDoP)
		convert, off := optionOf(t, v, dsdstate.ModeConvert), optionOf(t, v, dsdstate.ModeOff)
		for _, o := range []playback.DSDOption{native, dop} {
			if !o.Offered || len(o.Rates) != 3 {
				t.Errorf("%s: %+v, want offered at the three rates", o.Mode, o)
			}
		}
		if r, _ := rateOf(native, playbacktest.DSD64); r.Evidence != (playback.DSDEvidence{Rung: "reported"}) {
			t.Errorf("native at DSD64: %+v, want reported", r)
		}
		if dop.NeedsConfirmation || dop.Blocked != "" {
			t.Errorf("DoP: needs confirmation %v, blocked %q; want neither, its confirmation counting and no gain stage", dop.NeedsConfirmation, dop.Blocked)
		}
		if want := (dsdconv.Plan{RateHz: 176400, ValidBits: 24}); !convert.Offered || convert.Plan == nil || *convert.Plan != want || convert.Rates != nil {
			t.Errorf("convert: %+v, want offered at %v and no rates", convert, want)
		}
		if !off.Offered || off.Rates != nil || off.Plan != nil {
			t.Errorf("off: %+v, want offered, nothing else", off)
		}
		if got := []string{d.Options[0].Mode, d.Options[1].Mode, d.Options[2].Mode, d.Options[3].Mode}; !slices.Equal(got, []string{"native", "dop", "convert", "off"}) || len(d.Options) != 4 {
			t.Errorf("the options in the order %v, want native, dop, convert, off", got)
		}
		for _, tr := range []string{dsdstate.TransportNative, dsdstate.TransportDoP} {
			if f := firstUseOf(t, v, tr); f.State != playback.FirstUseAcknowledged || f.BitRates != nil {
				t.Errorf("%s's first use %+v, want acknowledged at every rate", tr, f)
			}
		}
	})

	t.Run("an unknown DAC with no choice of its own", func(t *testing.T) {
		b := policyBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(dsdstate.Defaults()))
		b.RefreshDSD(true)
		p, v := viewOf(t, b)
		if v.Mode != dsdstate.ModeAuto || v.AllowDoP || len(p.Tuples) != 0 || len(v.Evidence) != 0 || len(v.Refused) != 0 || len(p.Converted) != 3 {
			t.Errorf("mode %q, allowDoP %v, tuples %+v, evidence %+v, refused %+v, converted %+v; want auto, no DoP allowed, every rate converted",
				v.Mode, v.AllowDoP, p.Tuples, v.Evidence, v.Refused, p.Converted)
		}
		d := v.DAC
		if d == nil || d.Profile != nil || d.Choice != (playback.DSDChoice{Mode: dsdstate.ModeAuto}) {
			t.Fatalf("the DAC %+v, want no profile and the bridge's default with no revision", d)
		}
		native, dop := optionOf(t, v, dsdstate.ModeNative), optionOf(t, v, dsdstate.ModeDoP)
		if !native.Offered || len(native.Rates) != 3 || native.Rates[0].Evidence != (playback.DSDEvidence{}) {
			t.Errorf("native: %+v, want offered at the three rates with no evidence", native)
		}
		if !dop.Offered || !dop.NeedsConfirmation || dop.Blocked != "" {
			t.Errorf("DoP: %+v, want offered, needing a confirmation, unblocked", dop)
		}
		for _, tr := range []string{dsdstate.TransportNative, dsdstate.TransportDoP} {
			if f := firstUseOf(t, v, tr); f.State != playback.FirstUsePending {
				t.Errorf("%s's first use %+v, want pending", tr, f)
			}
		}
	})

	t.Run("no DAC available", func(t *testing.T) {
		st := dsdstate.Defaults()
		st.DefaultMode = dsdstate.ModeOff
		st.Warnings = []dsdstate.Warning{{Code: dsdstate.WarningMigratedOff, At: playbacktest.T0},
			{Code: dsdstate.WarningMigrationRecordMissing, DAC: ptrKey(playbacktest.DACKey()), At: playbacktest.T0}}
		b := policyBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(st), func(p *playback.DSDPolicy) {
			p.Bound = func() (playback.BoundDAC, bool) { return playback.BoundDAC{}, false }
		})
		b.RefreshDSD(true)
		_, v := viewOf(t, b)
		if v.DAC != nil || v.Mode != dsdstate.ModeOff || v.AllowDoP {
			t.Errorf("the read side %+v, want no DAC, off", v)
		}
		var want []playback.DSDRefusal
		for _, r := range threeRates {
			want = append(want, playback.DSDRefusal{BitRate: r, Reason: string(playback.ReasonDACUnread)})
		}
		if !reflect.DeepEqual(v.Refused, want) {
			t.Errorf("refused %+v, want %+v", v.Refused, want)
		}
		if !reflect.DeepEqual(v.Warnings, []playback.DSDWarning{{Code: dsdstate.WarningMigratedOff}}) {
			t.Errorf("warnings %+v, want the bridge's alone: no DAC is bound to own the other", v.Warnings)
		}
	})

	t.Run("a state that does not read", func(t *testing.T) {
		b := policyBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(dsdstate.Refusing(playbacktest.T0)))
		b.RefreshDSD(true)
		_, v := viewOf(t, b)
		if v.Mode != dsdstate.ModeOff || v.AllowDoP || !reflect.DeepEqual(v.Warnings, []playback.DSDWarning{{Code: dsdstate.WarningStateUnreadable}}) {
			t.Errorf("mode %q, allowDoP %v, warnings %+v; want off and the damaged state's warning", v.Mode, v.AllowDoP, v.Warnings)
		}
		for i, r := range v.Refused {
			if r.Reason != dsdstate.RefusalStateUnreadable || r.BitRate != threeRates[i] {
				t.Errorf("refused %+v, want every rate refused as the state's", v.Refused)
			}
		}
		if v.DAC == nil || v.DAC.Choice != (playback.DSDChoice{Mode: dsdstate.ModeOff}) {
			t.Fatalf("the DAC %+v, want the bound DAC, off, no revision", v.DAC)
		}
		for _, mode := range []string{dsdstate.ModeNative, dsdstate.ModeDoP, dsdstate.ModeConvert} {
			if o := optionOf(t, v, mode); o.Offered {
				t.Errorf("%s offered over a state that does not read: %+v; the set establishes nothing of the DAC there", mode, o)
			}
		}
		if !optionOf(t, v, dsdstate.ModeOff).Offered {
			t.Error("off is not offered")
		}
	})
}

func ptrKey(k dacprofile.Key) *dacprofile.Key { return &k }

// TestAChangeThatMovesNoPlanIsPublishedInItsEpoch (binding note 8): a change
// of what the read side says while the set plays and admits what it did, a
// transport's first use observed, an evidence step, the choice's revision, a
// warning recorded, is published at the next refresh of an already-published
// backend, under the generation it was published at: the epoch a client's
// grant keys on (the design's section 7.1, tuples and converted "under its
// boot and epoch") moves only with what plays. A change that moves a plan
// still publishes at the next generation.
func TestAChangeThatMovesNoPlanIsPublishedInItsEpoch(t *testing.T) {
	st := playbacktest.ChoiceState(dsdstate.ModeNative)
	store := playbacktest.NewMemStore(st)
	b := policyBackend(t, playbacktest.NewBox(5), store, func(p *playback.DSDPolicy) { p.Profiles = playbacktest.KnownProfiles(nil) })
	b.RefreshDSD(true)
	first, v := viewOf(t, b)
	if firstUseOf(t, v, dsdstate.TransportNative).State != playback.FirstUseAcknowledged {
		t.Fatalf("the start's first use %+v", v.DAC.FirstUse)
	}
	step := func(what string, change func(d *dsdstate.DAC, st *dsdstate.State), check func(v playback.DSDView) bool) {
		t.Helper()
		next := store.State()
		for i := range next.DACs {
			if next.DACs[i].Key.Equal(playbacktest.DACKey()) {
				change(&next.DACs[i], &next)
			}
		}
		store.Set(next)
		b.RefreshDSD(true)
		p, v := viewOf(t, b)
		if !check(v) {
			t.Errorf("%s: the read side published %+v (DAC %+v)", what, v, v.DAC)
		}
		if p.Generation != first.Generation || p.BootID != first.BootID {
			t.Errorf("%s moved the epoch: generation %d, want %d", what, p.Generation, first.Generation)
		}
		if !reflect.DeepEqual(p.Tuples, first.Tuples) {
			t.Errorf("%s changed the tuples: %+v, want %+v", what, p.Tuples, first.Tuples)
		}
	}
	at := playbacktest.T0.Add(time.Hour)
	step("native's first use observed", func(d *dsdstate.DAC, _ *dsdstate.State) {
		for i := range d.FirstUse {
			if d.FirstUse[i].Transport == dsdstate.TransportNative {
				d.FirstUse[i].ObservedAt = at
			}
		}
	}, func(v playback.DSDView) bool {
		return firstUseOf(t, v, dsdstate.TransportNative).State == playback.FirstUseObserved
	})
	step("an evidence step", func(d *dsdstate.DAC, _ *dsdstate.State) {
		d.Evidence = append(d.Evidence, dsdstate.Evidence{Transport: dsdstate.TransportNative, Format: string(format.DSDU32BE), BitRate: playbacktest.DSD64,
			BCDDevice: playbacktest.DACBCD, StreamingObservedAt: at})
	}, func(v playback.DSDView) bool {
		r, _ := rateOf(optionOf(t, v, dsdstate.ModeNative), playbacktest.DSD64)
		return len(v.Evidence) == 3 && v.Evidence[0] == (playback.DSDEvidence{Rung: "streaming_observed", On: "2026-11-06"}) && r.Evidence == v.Evidence[0]
	})
	step("the choice's revision", func(d *dsdstate.DAC, _ *dsdstate.State) {
		c := *d.Choice
		c.Revision, c.SetAt, c.SetBy = 2, at, dsdstate.ByConsole
		d.Choice = &c
	}, func(v playback.DSDView) bool {
		return v.DAC.Choice.Revision == 2 && v.DAC.Choice.Mode == dsdstate.ModeNative
	})
	step("a warning", func(_ *dsdstate.DAC, st *dsdstate.State) {
		st.Warnings = append(st.Warnings, dsdstate.Warning{Code: dsdstate.WarningMigratedOff, At: at})
	}, func(v playback.DSDView) bool {
		return reflect.DeepEqual(v.Warnings, []playback.DSDWarning{{Code: dsdstate.WarningMigratedOff}})
	})

	// A change of what plays publishes the next generation.
	off := store.State()
	off.DACs[0].Choice.Mode = dsdstate.ModeOff
	store.Set(off)
	b.RefreshDSD(true)
	p, v := viewOf(t, b)
	if p.Generation != first.Generation+1 || len(p.Tuples) != 0 || v.Mode != dsdstate.ModeOff {
		t.Errorf("an off chosen published generation %d with %+v (mode %q), want %d, nothing playing, off", p.Generation, p.Tuples, v.Mode, first.Generation+1)
	}
}

// TestTheEvidenceIsThePolicysRung (binding note 10): the read side publishes
// the rung and the date the policy computed, never one rebuilt from the
// state: a probe's acceptance this run holds in its cache, with no durable
// record yet, is driver_accepted with no date; a durable acceptance under
// another kernel, or another firmware, is no rung of this box's, so the
// profile's report stands; one under the running kernel, a streaming
// observed and a listener's clean answer each publish their own word and
// date, the latest listener answer standing.
func TestTheEvidenceIsThePolicysRung(t *testing.T) {
	native64 := alsapcm.ProbeCandidate{Format: format.DSDU32BE, RateHz: playbacktest.DSD64 / 32}
	accepted := func(p *playback.DSDPolicy) {
		u := alsapcm.ProbeUnit{Model: playbacktest.DACKey().Canonical(), Firmware: playbacktest.DACBCD, Kernel: playbacktest.Kernel, Enumeration: "test/2"}
		p.Probes.Record(p.Probes.Begin(u), alsapcm.ProbeReport{Opened: true, Results: []alsapcm.ProbeResult{{Candidate: native64, Answer: alsapcm.ProbeAccepted, Pause: true}}})
	}
	evidence := func(e dsdstate.Evidence) dsdstate.State {
		st := playbacktest.ChoiceState(dsdstate.ModeNative)
		e.Transport, e.Format, e.BitRate = dsdstate.TransportNative, string(format.DSDU32BE), playbacktest.DSD64
		if e.BCDDevice == "" {
			e.BCDDevice = playbacktest.DACBCD
		}
		st.DACs[0].Evidence = []dsdstate.Evidence{e}
		return st
	}
	answer := func(answers ...string) dsdstate.State {
		st := playbacktest.ChoiceState(dsdstate.ModeAuto)
		for i, a := range answers {
			st.DACs[0].Listener = []dsdstate.Answer{{Transport: dsdstate.TransportNative, BitRate: playbacktest.DSD64, BCDDevice: playbacktest.DACBCD, Answer: a, On: []string{"2026-11-01", "2026-11-02"}[i]}}
		}
		return st
	}
	for _, tc := range []struct {
		name  string
		st    dsdstate.State
		known bool
		edit  func(*playback.DSDPolicy)
		want  playback.DSDEvidence
	}{
		{"a probe's acceptance in the cache alone", playbacktest.ChoiceState(dsdstate.ModeNative), false, accepted, playback.DSDEvidence{Rung: "driver_accepted"}},
		{"a durable acceptance under another kernel", evidence(dsdstate.Evidence{DriverAcceptedAt: playbacktest.T0, Kernel: "5.10.0-older"}), true, nil, playback.DSDEvidence{Rung: "reported"}},
		{"a durable acceptance under another firmware", evidence(dsdstate.Evidence{DriverAcceptedAt: playbacktest.T0, Kernel: playbacktest.Kernel, BCDDevice: "0200"}), true, nil, playback.DSDEvidence{Rung: "reported"}},
		{"a durable acceptance with no profile and no probe", evidence(dsdstate.Evidence{DriverAcceptedAt: playbacktest.T0, Kernel: "5.10.0-older"}), false, nil, playback.DSDEvidence{}},
		{"a durable acceptance under the running kernel", evidence(dsdstate.Evidence{DriverAcceptedAt: playbacktest.T0, Kernel: playbacktest.Kernel}), false, nil, playback.DSDEvidence{Rung: "driver_accepted", On: "2026-11-06"}},
		{"streaming observed", evidence(dsdstate.Evidence{StreamingObservedAt: playbacktest.T0}), false, nil, playback.DSDEvidence{Rung: "streaming_observed", On: "2026-11-06"}},
		{"a listener's clean answer", answer(dsdstate.AnswerClean), false, nil, playback.DSDEvidence{Rung: "listener_confirmed", On: "2026-11-01"}},
		{"a clean answer a noise one replaced", answer(dsdstate.AnswerClean, dsdstate.AnswerNoise), true, nil, playback.DSDEvidence{Rung: "reported"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := policyBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(tc.st), func(p *playback.DSDPolicy) {
				if tc.known {
					p.Profiles = playbacktest.KnownProfiles(nil)
				}
				if tc.edit != nil {
					tc.edit(p)
				}
			})
			b.RefreshDSD(true)
			p, v := viewOf(t, b)
			r, ok := rateOf(optionOf(t, v, dsdstate.ModeNative), playbacktest.DSD64)
			if !ok || r.Evidence != tc.want {
				t.Errorf("native at DSD64 carries %+v (offered %v), want %+v", r.Evidence, ok, tc.want)
			}
			for i, tu := range p.Tuples {
				if tu.Tuple.BitRate == playbacktest.DSD64 && tu.Tuple.Transport == "" && v.Evidence[i] != tc.want {
					t.Errorf("the native DSD64 tuple carries %+v, want %+v", v.Evidence[i], tc.want)
				}
			}
			for _, e := range v.Evidence {
				if e.Rung != "" && !slices.Contains([]string{"reported", "driver_accepted", "streaming_observed", "listener_confirmed"}, e.Rung) {
					t.Errorf("a rung %q the ladder does not name", e.Rung)
				}
			}
		})
	}
}
