package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// The read side of the player's DSD on GET /v1/outputs (B728 B8; the
// design's section 7.1): the element's dsdPlayback, dsdDAC and playback,
// and its dsdMode and allowDoP, from one snapshot of the backend's
// (playback.Backend.Facts), which carries the set published and what it says
// of the bound DAC.

// readSideBackend is a backend whose DSD is the effective policy over box
// and store, the test DAC bound at card 5, edit applied to the policy first,
// its set computed once.
func readSideBackend(t *testing.T, box *playbacktest.Box, store playback.DSDStore, edit ...func(*playback.DSDPolicy)) *playback.Backend {
	t.Helper()
	p := playbacktest.Policy(5, box, store)
	for _, e := range edit {
		e(p)
	}
	b, err := playback.New(playback.Config{DSD: p, Resolver: dopContents{}, Locator: refusedNativeLocator{new(int)}})
	if err != nil {
		t.Fatal(err)
	}
	b.RefreshDSD(true)
	return b
}

// readSideFacts are the element's facts of a ready player running b, its
// boot binding device, as serve's view reads them.
func readSideFacts(t *testing.T, b *playback.Backend, device config.PlayerDevice) api.BridgeOutputFacts {
	t.Helper()
	return readSideOutput(t, b, device, true).Facts(context.Background())
}

// readSideOutput is serve's view of a player running b, ready or not.
func readSideOutput(t *testing.T, b *playback.Backend, device config.PlayerDevice, ready bool) *bridgeOutput {
	t.Helper()
	if runtime.GOOS == "windows" {
		// As dsdFactsOf: the element is listed for a player with an identity,
		// which only the durable writer writes, and Windows has no directory
		// fsync; the player runs on Linux alone.
		t.Skip("the player's identity is written durably, which Windows cannot do, and the player whose element this is runs on Linux alone")
	}
	persistentMedium(t)
	readyCh := make(chan struct{})
	if ready {
		close(readyCh)
	}
	cfg := playerOutputConfig(t.TempDir(), nil)
	seedPlayerUUID(t, cfg.DataDir, thePlayerUUID)
	p := &bridgePlayer{ready: readyCh, device: device}
	if b != nil {
		p.be = b
	}
	return newBridgeOutput(servePlayerPlan{lists: true, start: true}, p, cfg)
}

// staticBridgeOutput is serve's player answering the route with f.
type staticBridgeOutput struct{ f api.BridgeOutputFacts }

func (s staticBridgeOutput) Enabled() bool                               { return true }
func (s staticBridgeOutput) Facts(context.Context) api.BridgeOutputFacts { return s.f }

// outputsBodyOf is GET /v1/outputs' body for a bridge whose player has the
// facts f and whose MPD gate is shut: the element as the route builds it.
func outputsBodyOf(t *testing.T, f api.BridgeOutputFacts) []byte {
	t.Helper()
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), tokensFileName))
	if err != nil {
		t.Fatal(err)
	}
	bearer, _, err := store.Mint("phone")
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(&config.Config{ListenAddress: ":7788", LibraryName: "T", LibraryRoots: []string{t.TempDir()}}, store, nil, "fp").
		WithLocalOutputs(api.LocalOutputsConfig{GOOS: "linux", Output: func() config.OutputConfig { return config.OutputConfig{MPD: "off"} }}).
		WithBridgeOutput(staticBridgeOutput{f})
	req := httptest.NewRequest(http.MethodGet, "/v1/outputs", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/outputs: %d %s", w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}

// bridgeObjectOf is the bridge object of body's one element.
func bridgeObjectOf(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var top struct {
		Outputs []map[string]json.RawMessage `json:"outputs"`
	}
	if err := json.Unmarshal(body, &top); err != nil || len(top.Outputs) != 1 {
		t.Fatalf("%s: %v", body, err)
	}
	return rawKeys(t, top.Outputs[0]["bridge"])
}

// optionOf is d's option for mode.
func optionOf(t *testing.T, d *api.OutputDSDDAC, mode string) api.OutputDSDOption {
	t.Helper()
	if d == nil {
		t.Fatalf("no dsdDAC, so no option for %s", mode)
	}
	for _, o := range d.Options {
		if o.Mode == mode {
			return o
		}
	}
	t.Fatalf("the options %+v hold no %s", d.Options, mode)
	return api.OutputDSDOption{}
}

// firstUseIn is d's first use of transport.
func firstUseIn(t *testing.T, d *api.OutputDSDDAC, transport string) api.OutputDSDFirstUse {
	t.Helper()
	if d == nil {
		t.Fatalf("no dsdDAC, so no first use of %s", transport)
	}
	for _, f := range d.FirstUse {
		if f.Transport == transport {
			return f
		}
	}
	t.Fatalf("the first uses %+v hold no %s", d.FirstUse, transport)
	return api.OutputDSDFirstUse{}
}

// countingStore is a DSD store whose reads are counted.
type countingStore struct {
	playback.DSDStore
	reads atomic.Int32
}

func (c *countingStore) State() dsdstate.State {
	c.reads.Add(1)
	return c.DSDStore.State()
}

// TestTheBridgeElementCarriesTheDACChoice (B728 B8; the design's section
// 7.1; binding notes 4 and 7): the player's element carries the bound DAC's
// DSD as the set published says it, its key, profile, choice and revision,
// options and first use, and dsdMode and allowDoP describe that DAC's choice
// from the same snapshot: with no choice of its own the bridge's default;
// its own choice, in a state seeded through the store's own writer and read
// back from the file, or one the migration of an owner's record made; off,
// allowing no DoP, while the backend is not ready, no DAC is bound or no DAC
// is available. The route reads the snapshot alone: no read of the state's
// store, and the state's file replaced, changes nothing it says. A first use
// observed on an already-published backend is published in its epoch.
func TestTheBridgeElementCarriesTheDACChoice(t *testing.T) {
	t.Run("no choice recorded", func(t *testing.T) {
		f := readSideFacts(t, readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(dsdstate.Defaults())), config.PlayerDevice{})
		if f.DSDMode != dsdstate.ModeAuto || f.AllowDoP {
			t.Errorf("dsdMode %q, allowDoP %v; want the bridge's default, auto, and no DoP allowed on an unknown DAC", f.DSDMode, f.AllowDoP)
		}
		d := f.DSDDAC
		if d == nil || d.DACKey != playbacktest.DACKey().DACKey() || d.Profile != nil || d.Choice != (api.OutputDSDChoice{Mode: dsdstate.ModeAuto}) {
			t.Fatalf("dsdDAC %+v, want the test DAC's key, no profile, auto with no revision", d)
		}
		if got := []string{d.Options[0].Mode, d.Options[1].Mode, d.Options[2].Mode, d.Options[3].Mode}; len(d.Options) != 4 || !slices.Equal(got, []string{"native", "dop", "convert", "off"}) {
			t.Errorf("the options %+v, want native, dop, convert and off", d.Options)
		}
		if n := optionOf(t, d, "native"); !n.Offered || n.Rates == nil || len(*n.Rates) != 3 || n.NeedsConfirmation != nil {
			t.Errorf("native's option %+v, want offered at the three rates", n)
		}
		if o := optionOf(t, d, "dop"); !o.Offered || o.NeedsConfirmation == nil || !*o.NeedsConfirmation || o.Blocked != "" {
			t.Errorf("DoP's option %+v, want offered, needing a confirmation, unblocked", o)
		}
		if c := optionOf(t, d, "convert"); !c.Offered || c.RateHz != 176400 || c.ValidBits != 24 || c.PassbandHz != 30000 || c.Dither != "none" || c.Rates != nil {
			t.Errorf("convert's option %+v, want its plan, 176.4 kHz at 24 bits, 30 kHz, no dither", c)
		}
		for _, tr := range []string{"native", "dop"} {
			if fu := firstUseIn(t, d, tr); fu.State != playback.FirstUsePending || fu.BitRates != nil {
				t.Errorf("%s's first use %+v, want pending", tr, fu)
			}
		}
	})

	t.Run("a choice recorded in a seeded state file", func(t *testing.T) {
		persistentMedium(t)
		dir := t.TempDir()
		seed, err := dsdstate.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := seed.Update(func(st *dsdstate.State) error { *st = playbacktest.ChoiceState(dsdstate.ModeNative); return nil }); err != nil {
			t.Fatal(err)
		}
		file, err := dsdstate.Open(dir)
		if err != nil || file.Report().Outcome != dsdstate.Loaded {
			t.Fatalf("the seeded state's file: %+v, %v", file.Report(), err)
		}
		store := &countingStore{DSDStore: file}
		b := readSideBackend(t, playbacktest.NewBox(5), store, func(p *playback.DSDPolicy) { p.Profiles = playbacktest.KnownProfiles(nil) })
		reads := store.reads.Load()
		out := readSideOutput(t, b, config.PlayerDevice{}, true)
		f := out.Facts(context.Background())
		if f.DSDMode != dsdstate.ModeNative || !f.AllowDoP {
			t.Errorf("dsdMode %q, allowDoP %v; want the DAC's own native and DoP allowed by its profile", f.DSDMode, f.AllowDoP)
		}
		d := f.DSDDAC
		if d == nil || d.Choice != (api.OutputDSDChoice{Mode: dsdstate.ModeNative, Revision: 1, DoPConfirmed: true}) || d.Profile == nil || d.Profile.ID != "test.dac" {
			t.Fatalf("dsdDAC %+v, want the seeded native at revision 1, DoP confirmed, the test profile", d)
		}
		for _, tr := range []string{"native", "dop"} {
			if fu := firstUseIn(t, d, tr); fu.State != playback.FirstUseAcknowledged || fu.BitRates != nil {
				t.Errorf("%s's first use %+v, want acknowledged at every rate", tr, fu)
			}
		}
		if n := store.reads.Load(); n != reads {
			t.Errorf("the route read the player's DSD state %d times: it reads the set published alone", n-reads)
		}
		if err := os.WriteFile(filepath.Join(dir, dsdstate.FileName), []byte("{not a state"), 0o600); err != nil {
			t.Fatal(err)
		}
		if again := out.Facts(context.Background()); !reflect.DeepEqual(again.DSDDAC, f.DSDDAC) || again.DSDMode != f.DSDMode || !reflect.DeepEqual(again.DSDPlayback, f.DSDPlayback) {
			t.Errorf("the state's file replaced, the element says %+v / %q, want what the set published says, %+v / %q", again.DSDDAC, again.DSDMode, f.DSDDAC, f.DSDMode)
		}
	})

	t.Run("a migrated choice", func(t *testing.T) {
		st := playbacktest.MigratedState(dsdstate.ModeNative, false, []int{playbacktest.DSD64}, nil)
		f := readSideFacts(t, readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(st)), config.PlayerDevice{})
		d := f.DSDDAC
		if f.DSDMode != dsdstate.ModeNative || f.AllowDoP || d == nil || d.Choice != (api.OutputDSDChoice{Mode: dsdstate.ModeNative, Revision: 1}) {
			t.Fatalf("dsdMode %q, allowDoP %v, dsdDAC %+v; want the migration's native at revision 1, no DoP", f.DSDMode, f.AllowDoP, d)
		}
		if fu := firstUseIn(t, d, "native"); fu.State != playback.FirstUseAcknowledged || !slices.Equal(fu.BitRates, []int{playbacktest.DSD64}) {
			t.Errorf("native's first use %+v, want acknowledged at the record's DSD64 alone", fu)
		}
		if fu := firstUseIn(t, d, "dop"); fu.State != playback.FirstUsePending {
			t.Errorf("DoP's first use %+v, want pending", fu)
		}
		p := f.DSDPlayback
		want := []api.OutputDSDRefused{{BitRate: playbacktest.DSD128, Reason: "first_use_pending"}, {BitRate: playbacktest.DSD256, Reason: "first_use_pending"}}
		if p == nil || len(p.Tuples) != 1 || p.Tuples[0].BitRate != playbacktest.DSD64 || p.Tuples[0].Evidence != "listener_confirmed" || p.Tuples[0].EvidenceOn != "2026-10-01" || !reflect.DeepEqual(p.Refused, want) {
			t.Errorf("dsdPlayback %+v, want DSD64 native, listener confirmed on the record's date, and the rest refused for their first use", p)
		}
	})

	t.Run("the backend not ready", func(t *testing.T) {
		b := readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative)))
		f := readSideOutput(t, b, config.PlayerDevice{}, false).Facts(context.Background())
		if !f.Listed || f.DSDMode != dsdstate.ModeOff || f.AllowDoP || f.DSDPlayback != nil || f.DSDDAC != nil || f.Playback != nil {
			t.Errorf("the element before the backend is ready: %+v; want it listed, off, no DoP and no DSD object", f)
		}
	})

	t.Run("no DAC bound", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("the player's identity is written durably, which Windows cannot do")
		}
		persistentMedium(t)
		cfg := playerOutputConfig(t.TempDir(), func(p *config.PlayerConfig) { p.Device = config.PlayerDevice{} })
		seedPlayerUUID(t, cfg.DataDir, thePlayerUUID)
		ready := make(chan struct{})
		close(ready)
		plan := planServePlayer(cfg, "linux", true)
		f := newBridgeOutput(plan, &bridgePlayer{ready: ready}, cfg).Facts(context.Background())
		if f.State != bridgeStateUnavailable || f.Reason != refusedNotConfigured || f.DSDMode != dsdstate.ModeOff || f.AllowDoP || f.DSDPlayback != nil || f.DSDDAC != nil {
			t.Errorf("the element with no DAC bound: %+v; want not_configured, off, no DoP and no DSD object", f)
		}
	})

	t.Run("no DAC available", func(t *testing.T) {
		b := readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative)), func(p *playback.DSDPolicy) {
			p.Bound = func() (playback.BoundDAC, bool) { return playback.BoundDAC{}, false }
		})
		f := readSideFacts(t, b, config.PlayerDevice{})
		if f.DSDMode != dsdstate.ModeOff || f.AllowDoP || f.DSDDAC != nil || f.DSDPlayback == nil || len(f.DSDPlayback.Refused) != 3 {
			t.Errorf("the element with no DAC available: dsdMode %q, allowDoP %v, dsdDAC %+v, dsdPlayback %+v; want off, no DAC, every rate refused", f.DSDMode, f.AllowDoP, f.DSDDAC, f.DSDPlayback)
		}
	})

	t.Run("a first use observed on a published backend", func(t *testing.T) {
		store := playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative))
		b := readSideBackend(t, playbacktest.NewBox(5), store, func(p *playback.DSDPolicy) { p.Profiles = playbacktest.KnownProfiles(nil) })
		out := readSideOutput(t, b, config.PlayerDevice{}, true)
		before := out.Facts(context.Background())
		st := store.State()
		st.DACs[0].FirstUse[0].ObservedAt = playbacktest.T0.Add(time.Minute)
		store.Set(st)
		b.RefreshDSD(true)
		after := out.Facts(context.Background())
		if fu := firstUseIn(t, after.DSDDAC, "native"); fu.State != playback.FirstUseObserved {
			t.Errorf("native's first use %+v after it was observed, want observed", fu)
		}
		if after.DSDPlayback.PolicyGeneration != before.DSDPlayback.PolicyGeneration || !reflect.DeepEqual(after.DSDPlayback.Tuples, before.DSDPlayback.Tuples) {
			t.Errorf("a first use observed moved the epoch: generation %d, want %d", after.DSDPlayback.PolicyGeneration, before.DSDPlayback.PolicyGeneration)
		}
	})
}

// s16At44 is a playback altset of S16_LE at 44.1 kHz alone.
func s16At44() alsa.Altset {
	one, two, bits := 1, 2, 16
	return alsa.Altset{Interface: &one, Altset: &one, Formats: []string{"S16_LE"}, Channels: &two, Rates: []int{44100}, Bits: &bits}
}

// convertedObservation is the observation of an occurrence of DSD at
// fromBitRate the set policy converts, its conversion having saturated
// clipped samples, as the backend makes it (B728 B11's observeOccurrence,
// whose values TestTheClaimsAreThePlans pins against the real backend): the
// converted rate's plan, its PCM in the plan's packing, no logical rate, no
// policy ID and no packer.
func convertedObservation(t *testing.T, policy playback.PlayerPolicy, fromBitRate int, clipped int64) playback.PlaybackFacts {
	t.Helper()
	for _, r := range policy.Converted {
		if r.BitRate != fromBitRate {
			continue
		}
		return playback.PlaybackFacts{BootID: policy.BootID, PolicyGeneration: policy.Generation, Session: 3, Occurrence: 7, State: "open",
			Format: string(r.Packing), RateHz: r.Plan.RateHz, Channels: 2, ValidBits: r.Plan.ValidBits, Transport: playback.TransportConverted,
			ObservedAt: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC),
			Converted:  &playback.ConvertedPlayback{FromBitRate: fromBitRate, Filter: r.Plan.Filter(), PassbandHz: r.Plan.PassbandHz(), Dither: r.Plan.Dither(), GainDB: r.Plan.GainDB(), ClippedSamples: clipped}}
	}
	t.Fatalf("the set %+v converts no %d", policy.Converted, fromBitRate)
	return playback.PlaybackFacts{}
}

// TestAConvertedEntryCarriesItsPlan (B728 B8; the design's sections 4.8 and
// 7.1; binding notes 10 and 12): each rate the set converts is published
// with the plan it converts by, its rate, valid bits, passband, dither and
// filter, whichever target the DAC takes (176.4 kHz at 24 bits, 30 kHz and no
// dither; 44.1 kHz at 16, 20 kHz and TPDF), and so is convert's option; a
// converted occurrence's observation says converted, its PCM's format, rate
// and valid bits, logical rate 0, and the plan with the source's rate and the
// samples saturated as the observation counted them, read from it and never
// computed again; and no converted rate or occurrence carries an evidence
// word, though the DAC's native option at that rate has one.
func TestAConvertedEntryCarriesItsPlan(t *testing.T) {
	for _, tc := range []struct {
		name    string
		altsets []alsa.Altset
		plan    api.OutputDSDConverted
		packing string
	}{
		{"a DAC that takes 176.4 kHz at 24 bits", playbacktest.AllAltsets(), api.OutputDSDConverted{RateHz: 176400, ValidBits: 24, PassbandHz: 30000, Dither: "none", Filter: "dsdconv-1"}, "S24_3LE"},
		{"a DAC that takes 44.1 kHz at 16 bits alone", []alsa.Altset{s16At44()}, api.OutputDSDConverted{RateHz: 44100, ValidBits: 16, PassbandHz: 20000, Dither: "tpdf", Filter: "dsdconv-1"}, "S16_LE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := playbacktest.NewBox(5)
			box.SetAltsets(5, 0, tc.altsets...)
			// What was observed of native DSD64 on this box, before its choice
			// went back to the bridge's auto, which converts on an unknown DAC.
			st := dsdstate.Defaults()
			st.DACs = []dsdstate.DAC{{Key: playbacktest.DACKey(), Evidence: []dsdstate.Evidence{{Transport: dsdstate.TransportNative, Format: string(format.DSDU32BE),
				BitRate: playbacktest.DSD64, BCDDevice: playbacktest.DACBCD, StreamingObservedAt: playbacktest.T0}}}}
			b := readSideBackend(t, box, playbacktest.NewMemStore(st))
			f := readSideFacts(t, b, config.PlayerDevice{})
			var want []api.OutputDSDConverted
			for _, r := range []int{playbacktest.DSD64, playbacktest.DSD128, playbacktest.DSD256} {
				e := tc.plan
				e.BitRate = r
				want = append(want, e)
			}
			if p := f.DSDPlayback; p == nil || !reflect.DeepEqual(p.Converted, want) || len(p.Tuples) != 0 || len(p.Refused) != 0 {
				t.Errorf("dsdPlayback %+v, want every rate converted by %+v, no tuple and nothing refused", p, tc.plan)
			}
			if c := optionOf(t, f.DSDDAC, "convert"); !c.Offered || c.RateHz != tc.plan.RateHz || c.ValidBits != tc.plan.ValidBits || c.PassbandHz != tc.plan.PassbandHz || c.Dither != tc.plan.Dither {
				t.Errorf("convert's option %+v, want the plan %+v", c, tc.plan)
			}
			policy, _ := b.Facts()
			live := convertedObservation(t, policy, playbacktest.DSD64, 17)
			_, _, out := bridgeDSDFacts(policy, &live, config.PlayerDevice{})
			wantOut := api.OutputPlayback{BootID: policy.BootID, PolicyGeneration: policy.Generation, Session: 3, Occurrence: 7, State: "open", Format: tc.packing,
				RateHz: tc.plan.RateHz, ValidBits: tc.plan.ValidBits, Channels: 2, Transport: "converted",
				Converted: &api.OutputPlaybackConverted{FromBitRate: playbacktest.DSD64, Filter: "dsdconv-1", PassbandHz: tc.plan.PassbandHz, Dither: tc.plan.Dither, ClippedSamples: 17}}
			if out == nil {
				t.Fatal("no observation of the converted occurrence is published")
			}
			got := *out
			got.ObservedAt = nil
			if !reflect.DeepEqual(got, wantOut) {
				t.Errorf("the observation %+v (converted %+v), want %+v (converted %+v)", got, got.Converted, wantOut, wantOut.Converted)
			}
			obj := bridgeObjectOf(t, outputsBodyOf(t, api.BridgeOutputFacts{Listed: true, UUID: thePlayerUUID, State: "playing", Codecs: f.Codecs, DSDPlayback: f.DSDPlayback, DSDDAC: f.DSDDAC, Playback: out}))
			playbackObj := rawKeys(t, obj["playback"])
			if string(playbackObj["logicalRate"]) != "0" || string(playbackObj["transport"]) != `"converted"` {
				t.Errorf("the observation %s, want transport converted and a logical rate of 0, present", obj["playback"])
			}
			for _, key := range []string{"evidence", "evidenceOn", "packingVersion", "policyId"} {
				if v, ok := playbackObj[key]; ok {
					t.Errorf("a converted occurrence's observation carries %s %s", key, v)
				}
			}
			var entries []map[string]json.RawMessage
			if err := json.Unmarshal(rawKeys(t, obj["dsdPlayback"])["converted"], &entries); err != nil || len(entries) != 3 {
				t.Fatalf("the converted entries %s: %v", obj["dsdPlayback"], err)
			}
			for _, e := range entries {
				for key := range e {
					if strings.HasPrefix(key, "evidence") {
						t.Errorf("a converted entry carries %s: %v", key, e)
					}
				}
			}
			if tc.packing == "S24_3LE" {
				n := optionOf(t, f.DSDDAC, "native")
				if n.Rates == nil || len(*n.Rates) == 0 || (*n.Rates)[0].Evidence != "streaming_observed" {
					t.Errorf("native's option %+v, want DSD64's streaming observed, which no converted rate takes", n)
				}
			}
		})
	}
}

// TestAMigrationFailureIsPublished (B728 B8; the design's sections 2.5, 3.6
// and 7.1): what the migration of the legacy keys could not carry over is
// published in dsdPlayback's warnings, each code with its reason word: the
// bridge's warnings whatever DAC is bound, and a DAC's own while that DAC is
// the bound one; and through serve, an owner's record the legacy reader
// refuses as stale is published as migration_record_refused (stale_version).
func TestAMigrationFailureIsPublished(t *testing.T) {
	other := func() *dacprofile.Key {
		iface, product := 0, "Another DAC"
		k, err := dacprofile.NewKey(0x1234, 0x9999, &iface, &product)
		if err != nil {
			t.Fatal(err)
		}
		return &k
	}()
	bound := playbacktest.DACKey()
	st := dsdstate.Defaults()
	st.DefaultMode = dsdstate.ModeOff
	st.Warnings = []dsdstate.Warning{
		{Code: dsdstate.WarningMigratedOff, At: playbacktest.T0},
		{Code: dsdstate.WarningMigrationRecordRefused, Reason: dsdstate.ReasonStaleVersion, DAC: &bound, At: playbacktest.T0},
		{Code: dsdstate.WarningMigrationRecordMissing, DAC: other, At: playbacktest.T0},
	}
	f := readSideFacts(t, readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(st)), config.PlayerDevice{})
	want := []api.OutputDSDWarning{{Code: "migrated_off"}, {Code: "migration_record_refused", Reason: "stale_version"}}
	if f.DSDPlayback == nil || !reflect.DeepEqual(f.DSDPlayback.Warnings, want) {
		t.Errorf("the warnings %+v, want %+v: the bridge's, and the bound DAC's own", f.DSDPlayback, want)
	}
	raw := bridgeObjectOf(t, outputsBodyOf(t, f))
	if got := string(rawKeys(t, raw["dsdPlayback"])["warnings"]); got != `[{"code":"migrated_off"},{"code":"migration_record_refused","reason":"stale_version"}]` {
		t.Errorf("the warnings on the wire %s", got)
	}

	t.Run("through serve", func(t *testing.T) {
		needsDirectorySync(t)
		persistentMedium(t)
		box := newDSDBox(t)
		goBarDSD(t, box.trees)
		standIn := newPlayerStandIn()
		ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + "    dsdMode: native\n    dsdQualification: approved.json\n", standIn: standIn,
			before: func(dataDir string) {
				legacyRecordIn(t, dataDir, goBarLiveIdentity(t, box.trees), qualification.LegacyVersion-1, nativeAt(dsdstate.DSD64))
			}, with: box.with})
		availableOutput(t, ps.runningEngine(t))
		b := policyBackendOf(t, wiredPlayback(t, standIn), dopContents{}, &dopOpens{})
		f := dsdFactsOf(t, b)
		want := []api.OutputDSDWarning{{Code: "migration_record_refused", Reason: "stale_version"}}
		if f.DSDPlayback == nil || !reflect.DeepEqual(f.DSDPlayback.Warnings, want) {
			t.Errorf("the warnings serve's player publishes: %+v, want %+v", f.DSDPlayback, want)
		}
		stopped(t, ps)
	})
}

// TestADamagedStateIsPublishedWithNoDAC (B728 B8; the design's sections 2.5
// and 7.1; binding note 6): a player's DSD state that did not read is
// published while no DAC is available, dsdPlayback carrying its
// state_unreadable warning and every rate refused for want of a reading of
// the DAC, and dsdDAC absent: from a state the store refused in memory, and
// through serve, from a damaged file with the GO bar unplugged.
func TestADamagedStateIsPublishedWithNoDAC(t *testing.T) {
	check := func(t *testing.T, f api.BridgeOutputFacts) {
		t.Helper()
		if f.DSDDAC != nil || f.DSDMode != dsdstate.ModeOff || f.AllowDoP {
			t.Errorf("dsdDAC %+v, dsdMode %q, allowDoP %v; want no DAC, off", f.DSDDAC, f.DSDMode, f.AllowDoP)
		}
		p := f.DSDPlayback
		if p == nil || !reflect.DeepEqual(p.Warnings, []api.OutputDSDWarning{{Code: "state_unreadable"}}) || len(p.Tuples) != 0 || len(p.Converted) != 0 {
			t.Fatalf("dsdPlayback %+v, want the damaged state's warning and nothing playing", p)
		}
		for i, r := range p.Refused {
			if r != (api.OutputDSDRefused{BitRate: []int{playbacktest.DSD64, playbacktest.DSD128, playbacktest.DSD256}[i], Reason: "dsd_dac_unread"}) {
				t.Errorf("refused %+v, want every rate refused for want of a reading of the DAC", p.Refused)
			}
		}
		raw := bridgeObjectOf(t, outputsBodyOf(t, f))
		if _, present := raw["dsdDAC"]; present {
			t.Errorf("the element carries dsdDAC with no DAC: %s", raw["dsdDAC"])
		}
		if _, present := raw["dsdPlayback"]; !present {
			t.Error("the element carries no dsdPlayback")
		}
	}
	check(t, readSideFacts(t, readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(dsdstate.Refusing(playbacktest.T0)), func(p *playback.DSDPolicy) {
		p.Bound = func() (playback.BoundDAC, bool) { return playback.BoundDAC{}, false }
	}), config.PlayerDevice{}))

	t.Run("through serve", func(t *testing.T) {
		needsDirectorySync(t)
		persistentMedium(t)
		box := newDSDBox(t)
		box.unplug(t)
		standIn := newPlayerStandIn()
		ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: standIn,
			before: func(dataDir string) {
				if err := os.WriteFile(filepath.Join(dataDir, dsdstate.FileName), []byte("{\"schemaVersion\": 9}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}, with: box.with})
		awaitEngine(t, ps.runningEngine(t), "the output known, the GO bar missing", func(s *engine.Snapshot) bool { return s.Output().Known && !s.Output().Available })
		check(t, dsdFactsOf(t, policyBackendOf(t, wiredPlayback(t, standIn), dopContents{}, &dopOpens{})))
		stopped(t, ps)
	})
}

// stringsIn are every string a JSON document holds, its members' names and
// values alike.
func stringsIn(t *testing.T, raw []byte) []string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for k, e := range x {
				out = append(out, k)
				walk(e)
			}
		}
	}
	walk(v)
	return out
}

// dacKeyForm is a dacKey's form: d1- and 16 lowercase hexadecimal digits.
var dacKeyForm = regexp.MustCompile(`^d1-[0-9a-f]{16}$`)

// TestNoDACKeyOrModelCarriesASerialOrAPort (B728 B8; binding note 11): no
// string of the player's DSD on GET /v1/outputs carries the bound unit's
// serial or its port, whatever the binding names, a port-only binding whose
// unit's product string holds its serial and whose manufacturer string its
// port included: the model is redacted with the serial and port the unit
// reports, and every other string with them and the binding's; the dacKey
// is d1- and 16 hexadecimal digits, the model key's where the product string
// holds neither, and no function of the serial where it does: two units of
// the model, their product strings each holding its own serial, publish one
// key. Through serve too, on the GO bar, bound by its port, its product
// string holding its serial.
func TestNoDACKeyOrModelCarriesASerialOrAPort(t *testing.T) {
	const port = "platform-xhci-hcd.0/usb2-1"
	unitWith := func(serial string) alsa.UnitModel {
		m := playbacktest.DACUnit()
		m.USB.Serial = usbid.New(serial)
		m.Product = "Test DAC " + serial
		m.Manufacturer = "Test " + port
		return m
	}
	profiles := &dacprofile.Database{SchemaVersion: dacprofile.SchemaVersion, Profiles: []dacprofile.Profile{{ID: "test.any", Name: "Test DAC",
		Match:   []dacprofile.Match{{VendorID: "1234", ProductID: "5678"}},
		Caveats: []dacprofile.Caveat{{Code: dacprofile.CaveatTransitionClicks, Transport: "dop", Note: "It clicks into DSD128."}}}}}
	byPort := config.PlayerDevice{Transport: "usb", Binding: config.BindingPort, Port: port}
	keyOf := func(serial string) string {
		box := playbacktest.NewBox(5)
		box.SetUnit(5, unitWith(serial))
		key, err := dsdstate.KeyOf(unitWith(serial).USB, unitWith(serial).Product)
		if err != nil {
			t.Fatal(err)
		}
		st := playbacktest.ChoiceState(dsdstate.ModeNative)
		st.DACs[0].Key = key
		st.Warnings = []dsdstate.Warning{{Code: dsdstate.WarningMigrationRecordMissing, DAC: &key, At: playbacktest.T0}}
		b := readSideBackend(t, box, playbacktest.NewMemStore(st), func(p *playback.DSDPolicy) {
			p.Profiles = profiles
			p.Bound = func() (playback.BoundDAC, bool) {
				return playback.BoundDAC{Card: 5, Identity: unitWith(serial).USB, Enumeration: "test/2"}, true
			}
		})
		f := readSideFacts(t, b, byPort)
		policy, _ := b.Facts()
		live := playback.PlaybackFacts{BootID: policy.BootID, PolicyGeneration: policy.Generation, Session: 1, Occurrence: 2, State: "open", Format: "DSD_U32_BE", RateHz: 88200,
			LogicalRate: playbacktest.DSD64, Channels: 2, ValidBits: 1, PolicyID: policy.Tuples[0].PolicyID, PackingVersion: 1, ObservedAt: playbacktest.T0}
		_, _, f.Playback = bridgeDSDFacts(policy, &live, byPort)
		body := outputsBodyOf(t, f)
		for _, s := range stringsIn(t, body) {
			if strings.Contains(s, serial) || strings.Contains(s, port) {
				t.Errorf("GET /v1/outputs carries %q, which holds the unit's serial or its port", s)
			}
		}
		if f.DSDPlayback.Model != "Test [redacted] Test DAC [redacted]" {
			t.Errorf("the model %q, want the unit's strings with its port and serial redacted", f.DSDPlayback.Model)
		}
		if f.DSDDAC == nil || !dacKeyForm.MatchString(f.DSDDAC.DACKey) {
			t.Fatalf("dsdDAC %+v, want a key of the form d1-<16 hex>", f.DSDDAC)
		}
		if f.DSDDAC.DACKey == key.DACKey() {
			t.Errorf("the key %s is the hash of a product string holding the serial", f.DSDDAC.DACKey)
		}
		if len(f.DSDPlayback.Warnings) != 1 || f.DSDDAC.Profile == nil || len(f.DSDDAC.Profile.Caveats) != 1 {
			t.Errorf("the warnings %+v and the profile %+v, want the DAC's own warning and the profile's caveat, each redacted", f.DSDPlayback.Warnings, f.DSDDAC.Profile)
		}
		return f.DSDDAC.DACKey
	}
	if a, b := keyOf("TESTDAC-0001"), keyOf("TESTDAC-0002"); a != b {
		t.Errorf("two units of one model, each with its serial in its product string, publish the keys %s and %s: the key is a function of the serial", a, b)
	}
	f := readSideFacts(t, readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(dsdstate.Defaults())), byPort)
	if f.DSDDAC == nil || f.DSDDAC.DACKey != playbacktest.DACKey().DACKey() {
		t.Errorf("a unit whose strings hold neither: dsdDAC %+v, want the model key's %s", f.DSDDAC, playbacktest.DACKey().DACKey())
	}

	t.Run("through serve, the GO bar bound by its port", func(t *testing.T) {
		needsDirectorySync(t)
		persistentMedium(t)
		box := newDSDBox(t)
		goBarDSD(t, box.trees)
		if err := os.WriteFile(filepath.Join(box.trees.sysfs, qualifyGoBar, "product"), []byte("GO bar "+goBarSerial), 0o600); err != nil {
			t.Fatal(err)
		}
		q, err := (alsa.SysfsReader{Root: box.trees.sysfs}).Qualification(5)
		if err != nil {
			t.Fatal(err)
		}
		place, err := usbid.ParsePort(q.Port)
		if err != nil {
			t.Fatal(err)
		}
		goBarPort := place.Place().String()
		standIn := newPlayerStandIn()
		ps := bootPlayerServe(t, playerServeCase{player: "    enabled: true\n    device:\n      binding: port\n      port: " + goBarPort + "\n", standIn: standIn, with: box.with})
		availableOutput(t, ps.runningEngine(t))
		b := policyBackendOf(t, wiredPlayback(t, standIn), dopContents{}, &dopOpens{})
		f := readSideFacts(t, b, config.PlayerDevice{Transport: "usb", Binding: config.BindingPort, Port: goBarPort})
		body := outputsBodyOf(t, f)
		for _, s := range stringsIn(t, body) {
			if strings.Contains(s, goBarSerial) || strings.Contains(s, goBarPort) || strings.Contains(s, q.Port) {
				t.Errorf("GET /v1/outputs carries %q, which holds the GO bar's serial or its port", s)
			}
		}
		if f.DSDPlayback == nil || !strings.Contains(f.DSDPlayback.Model, "[redacted]") || f.DSDDAC == nil || !dacKeyForm.MatchString(f.DSDDAC.DACKey) {
			t.Errorf("dsdPlayback %+v, dsdDAC %+v; want the model redacted and a key of its form", f.DSDPlayback, f.DSDDAC)
		}
		if !bytes.Contains(body, []byte(`"dsdDAC"`)) {
			t.Errorf("no dsdDAC: %s", body)
		}
		stopped(t, ps)
	})
}

// TestARetainedObservationKeepsItsOwnDACsEvidence (B728 B8 round 2; the
// design's section 7.1): the observation of a native occurrence that played
// on one DAC, retained once its session ended, keeps the evidence it was
// admitted with once the set published is another DAC's, whose tuple at its
// rate has its policy ID: another model the port binding found in the port,
// or the same model on another firmware. A policy ID names a tuple's
// transport, format, rate and timing, never the DAC, its firmware or the set,
// and the other DAC's tuple at listener_confirmed says nothing of what the
// first DAC played. Under the set it was admitted under, the observation
// takes its tuple's evidence as that set says it now: an evidence step
// publishes no new set.
func TestARetainedObservationKeepsItsOwnDACsEvidence(t *testing.T) {
	native, dsd64 := dsdstate.TransportNative, playbacktest.DSD64
	listened := func(bcd string) dsdstate.Answer {
		return dsdstate.Answer{Transport: native, BitRate: dsd64, BCDDevice: bcd, Answer: dsdstate.AnswerClean, On: "2026-11-02"}
	}
	otherModel := playbacktest.DACUnit()
	otherModel.USB.ID.Product, otherModel.Product = 0x5679, "Other DAC"
	otherFirmware := playbacktest.DACUnit()
	otherFirmware.BCDDevice = "0200"
	for _, c := range []struct {
		name string
		next alsa.UnitModel
		// state is the state once the next unit is bound: its own choice,
		// first use and a listener's clean answer at DSD64 under its firmware.
		state func(st dsdstate.State, key dacprofile.Key) dsdstate.State
	}{
		{"another DAC in the bound port", otherModel, func(st dsdstate.State, key dacprofile.Key) dsdstate.State {
			st.DACs = append(st.DACs, dsdstate.DAC{Key: key, Choice: &dsdstate.Choice{Mode: native, Revision: 1, SetAt: playbacktest.T0, SetBy: dsdstate.ByApp},
				FirstUse: []dsdstate.FirstUse{{Transport: native, BCDDevice: playbacktest.DACBCD, AcknowledgedAt: playbacktest.T0}},
				Listener: []dsdstate.Answer{listened(playbacktest.DACBCD)}})
			return st
		}},
		{"the same DAC on another firmware", otherFirmware, func(st dsdstate.State, _ dacprofile.Key) dsdstate.State {
			st.DACs[0].FirstUse = []dsdstate.FirstUse{{Transport: native, BCDDevice: "0200", AcknowledgedAt: playbacktest.T0}}
			st.DACs[0].Listener = []dsdstate.Answer{listened("0200")}
			return st
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			profiles := playbacktest.KnownProfiles(nil)
			other := profiles.Profiles[0]
			iface, product := 0, "Other DAC"
			other.ID, other.Match = "test.other", []dacprofile.Match{{VendorID: "1234", ProductID: "5679", Interface: &iface, Product: &product}}
			profiles.Profiles = append(profiles.Profiles, other)
			// The first DAC's DSD64 accepted by the driver under the running
			// kernel, before its occurrence played.
			st := playbacktest.ChoiceState(native)
			st.DACs[0].Evidence = []dsdstate.Evidence{{Transport: native, Format: string(format.DSDU32BE), BitRate: dsd64, BCDDevice: playbacktest.DACBCD,
				DriverAcceptedAt: playbacktest.T0, Kernel: playbacktest.Kernel}}
			store := playbacktest.NewMemStore(st)
			box := playbacktest.NewBox(5)
			bound := playbacktest.DACUnit()
			b := readSideBackend(t, box, store, func(p *playback.DSDPolicy) {
				p.Profiles = profiles
				p.Bound = func() (playback.BoundDAC, bool) {
					return playback.BoundDAC{Card: 5, Identity: bound.USB, Enumeration: "test/2"}, true
				}
			})
			first, _ := b.Facts()
			at := -1
			for i, tu := range first.Tuples {
				if tu.Tuple.BitRate == dsd64 && tu.Tuple.Transport == "" {
					at = i
				}
			}
			if at < 0 || first.DSD == nil || first.DSD.Evidence[at] != (playback.DSDEvidence{Rung: "driver_accepted", On: "2026-11-06"}) {
				t.Fatalf("the first DAC's set %+v (read side %+v): want a native DSD64 tuple, driver_accepted", first.Tuples, first.DSD)
			}
			// The observation the backend made of the occurrence that played
			// there, its session over.
			live := playback.PlaybackFacts{BootID: first.BootID, PolicyGeneration: first.Generation, Session: 1, Occurrence: 2, State: "released",
				Format: string(format.DSDU32BE), RateHz: dsd64 / 32, LogicalRate: dsd64, Channels: 2, ValidBits: 1, PolicyID: first.Tuples[at].PolicyID,
				PackingVersion: 1, ObservedAt: playbacktest.T0, Evidence: first.DSD.Evidence[at]}
			device := config.PlayerDevice{Transport: "usb", Binding: config.BindingPort, Port: "platform-xhci-hcd.0/usb2-1"}

			// An evidence step under the set it was admitted under: the
			// observation takes its tuple's rung as that set says it now.
			stepped := store.State()
			stepped.DACs[0].Evidence[0].StreamingObservedAt = playbacktest.T0
			store.Set(stepped)
			b.RefreshDSD(true)
			now, _ := b.Facts()
			if _, _, out := bridgeDSDFacts(now, &live, device); now.Generation != first.Generation || out.Evidence != "streaming_observed" || out.EvidenceOn != "2026-11-06" {
				t.Errorf("under its own set (generation %d, published %d) the observation says %q on %q, want streaming_observed, as the set says now",
					first.Generation, now.Generation, out.Evidence, out.EvidenceOn)
			}

			// The next unit bound, its own set published.
			key, err := dsdstate.KeyOf(c.next.USB, c.next.Product)
			if err != nil {
				t.Fatal(err)
			}
			store.Set(c.state(store.State(), key))
			box.SetUnit(5, c.next)
			bound = c.next
			b.RefreshDSD(true)
			next, _ := b.Facts()
			shared := false
			for i, tu := range next.Tuples {
				if tu.PolicyID == live.PolicyID {
					shared = next.DSD.Evidence[i] == (playback.DSDEvidence{Rung: "listener_confirmed", On: "2026-11-02"})
				}
			}
			if next.Generation == first.Generation || !shared {
				t.Fatalf("the next unit's set (generation %d after %d) %+v, read side %+v: want a tuple of the observation's policy ID, listener_confirmed",
					next.Generation, first.Generation, next.Tuples, next.DSD)
			}
			_, _, out := bridgeDSDFacts(next, &live, device)
			if out.Evidence != "driver_accepted" || out.EvidenceOn != "2026-11-06" {
				t.Errorf("the retained observation of the first DAC's occurrence says %q on %q under the next unit's set, want its own, driver_accepted",
					out.Evidence, out.EvidenceOn)
			}
		})
	}
}
