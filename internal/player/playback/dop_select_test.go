package playback

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// The rates a DSD policy names.
const (
	dsd64  = 2822400
	dsd128 = 5644800
	dsd256 = 11289600
)

// planKey names a plan by its transport, format, rate and policy.
func planKey(p engine.NativePlan) string {
	transport := p.Tuple.Transport
	if transport == "" {
		transport = "native"
	}
	return fmt.Sprintf("%s/%s/%d/%s", transport, p.Tuple.Format, p.Tuple.BitRate, p.PolicyID)
}

// TestDoPSelection: through the real reader, an approved DoP policy plays
// under dsdMode dop with allowDoP, and under auto at a rate where the
// record holds no native policy: the sink the backend opens is DoP's, S32_LE
// at the DoP carrier, under the approved policy; at a rate the record holds
// a native policy for, auto opens the native sink.
func TestDoPSelection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  string
		specs []policySpec
		// want is what each rate's content opens: the sink's kind and
		// format, keyed by the content's rate.
		want map[int]alsapcm.DSDConfig
	}{
		{"dop", "dop", []policySpec{nativeSpec("n64", "DSD_U32_BE", dsd64), dopSpec("dop64", dsd64)},
			map[int]alsapcm.DSDConfig{dsd64: {Kind: format.KindDoP, Format: format.S32LE, BitRate: dsd64}}},
		{"auto_absent_native", "auto", []policySpec{nativeSpec("n128", "DSD_U32_BE", dsd128), dopSpec("dop64", dsd64)},
			map[int]alsapcm.DSDConfig{dsd64: {Kind: format.KindDoP, Format: format.S32LE, BitRate: dsd64}, dsd128: {Kind: format.KindNativeDSD, Format: format.DSDU32BE, BitRate: dsd128}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := approvedDSDFixture(t, tc.mode, true, nil, tc.specs...)
			hw := &dsdHardware{}
			hw.counted(&cfg)
			cfg.Resolver = dsdFiles{"64": dsdContent(t, dsd64, 0), "128": dsdContent(t, dsd128, 0)}
			b, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			s := engine.SessionID(0)
			for _, rate := range []int{dsd64, dsd128} {
				want, plays := tc.want[rate]
				if !plays {
					continue
				}
				s++
				_ = startDSD(t, b, s, map[int]string{dsd64: "64", dsd128: "128"}[rate])
				native, pcm := hw.opens()
				if len(pcm) != 0 || len(native) != int(s) {
					t.Fatalf("DSD at %d: %d native and %d PCM opens, want the approved DSD sink opened", rate, len(native), len(pcm))
				}
				got := native[len(native)-1]
				if got.Kind != want.Kind || got.Format != want.Format || got.BitRate != want.BitRate || got.Pause != alsapcm.PauseKernel {
					t.Fatalf("DSD at %d opened %+v, want %+v under the approved policy", rate, got, want)
				}
			}
		})
	}
}

// TestDoPPublication: through the real reader, the published epoch is the
// effective set: the approved DoP policy under dop with allowDoP, under auto
// where no native policy holds its rate, and nothing, with its reason, where
// allowDoP is false.
func TestDoPPublication(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		allow      bool
		specs      []policySpec
		want       []string
		reason     string
	}{
		{"dop", "dop", true, []policySpec{dopSpec("dop64", dsd64), nativeSpec("n64", "DSD_U32_BE", dsd64)}, []string{"dop/S32_LE/2822400/dop64"}, ""},
		{"auto_absent_native", "auto", true, []policySpec{nativeSpec("n128", "DSD_U32_BE", dsd128), dopSpec("dop64", dsd64)}, []string{"dop/S32_LE/2822400/dop64", "native/DSD_U32_BE/5644800/n128"}, ""},
		{"dop_not_allowed", "dop", false, []policySpec{dopSpec("dop64", dsd64)}, nil, "dop_not_allowed"},
		{"auto_dop_not_allowed", "auto", false, []policySpec{dopSpec("dop64", dsd64)}, nil, "dop_not_allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := approvedDSDFixture(t, tc.mode, tc.allow, nil, tc.specs...)
			cfg.Resolver = edgeContent(nil)
			b, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			b.RefreshAuthorization(context.Background())
			p := b.Policy()
			var got []string
			for _, plan := range p.Tuples {
				got = append(got, planKey(plan))
			}
			if !slices.Equal(got, tc.want) || p.Reason != tc.reason || p.Revision != "r1" {
				t.Fatalf("published %v (reason %q, revision %q), want %v (reason %q, revision r1)", got, p.Reason, p.Revision, tc.want, tc.reason)
			}
		})
	}
}

// TestEffectiveSetMatchesPublication: one effective set over the mode and
// allowDoP feeds both selection and publication, through the real reader:
// the published tuples are each case's set, and at every rate the content
// plays as the first tuple published there, or is refused where none is.
func TestEffectiveSetMatchesPublication(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		allow      bool
		specs      []policySpec
		want       []string
	}{
		{"native", "native", false, []policySpec{dopSpec("dop64", dsd64), nativeSpec("le64", "DSD_U32_LE", dsd64), nativeSpec("be64", "DSD_U32_BE", dsd64)},
			[]string{"native/DSD_U32_BE/2822400/be64", "native/DSD_U32_LE/2822400/le64"}},
		{"dop", "dop", true, []policySpec{nativeSpec("n64", "DSD_U32_BE", dsd64), dopSpec("dop64", dsd64), nativeSpec("n128", "DSD_U16_LE", dsd128)},
			[]string{"dop/S32_LE/2822400/dop64"}},
		{"auto", "auto", true, []policySpec{dopSpec("dop64", dsd64), nativeSpec("n128", "DSD_U16_LE", dsd128), dopSpec("dop128", dsd128), dopSpec("dop256", dsd256)},
			[]string{"dop/S32_LE/2822400/dop64", "native/DSD_U16_LE/5644800/n128", "dop/S32_LE/11289600/dop256"}},
		{"auto_without_dop", "auto", false, []policySpec{dopSpec("dop64", dsd64), nativeSpec("n128", "DSD_U32_LE", dsd128)},
			[]string{"native/DSD_U32_LE/5644800/n128"}},
		{"dop_without_dop", "dop", false, []policySpec{dopSpec("dop64", dsd64)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := approvedDSDFixture(t, tc.mode, tc.allow, nil, tc.specs...)
			cfg.Resolver = edgeContent(nil)
			b, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			b.RefreshAuthorization(context.Background())
			var published []string
			for _, plan := range b.Policy().Tuples {
				published = append(published, planKey(plan))
			}
			if !slices.Equal(published, tc.want) {
				t.Fatalf("published %v, want %v", published, tc.want)
			}
			p, err := b.readPermission(context.Background())
			if err != nil {
				t.Fatalf("the approved record does not read as a permission under %s: %v", tc.mode, err)
			}
			for _, rate := range []int{dsd64, dsd128, dsd256} {
				first := ""
				for _, plan := range b.Policy().Tuples {
					if plan.Tuple.BitRate == rate {
						first = planKey(plan)
						break
					}
				}
				plan, err := p.selectNative(format.NativeDSD{BitRateHz: rate, Channels: 2})
				switch {
				case first == "" && err == nil:
					t.Errorf("at %d nothing is published and %s is selected", rate, planKey(plan))
				case first != "" && (err != nil || planKey(plan) != first):
					t.Errorf("at %d %s is published first and the selection is %+v, %v", rate, first, plan, err)
				}
			}
		})
	}
}

// TestDoPNegativePins: each refusal of the effective set is its own named
// reason, and refuses before the DAC is located to be opened, opened or
// written to; auto prefers the native policy at a rate the record holds one
// for, wherever the record lists it.
func TestDoPNegativePins(t *testing.T) {
	drifted := []alsactl.Control{gateControl("Native Gate", 1), gateControl("DoP Gate", 1)}
	for _, tc := range []struct {
		name, mode string
		allow      bool
		specs      []policySpec
		live       listControls
		content    []byte
		reason     engine.Reason
	}{
		{name: "auto_prefers_native", mode: "auto", allow: true, specs: []policySpec{dopSpec("dop64", dsd64), nativeSpec("n64", "DSD_U32_BE", dsd64)}},
		{name: "auto_allowdop_false", mode: "auto", specs: []policySpec{dopSpec("dop64", dsd64)}, reason: "dop_not_allowed"},
		{name: "dop_allowdop_false", mode: "dop", specs: []policySpec{dopSpec("dop64", dsd64)}, reason: "dop_not_allowed"},
		{name: "native_dop_only", mode: "native", specs: []policySpec{dopSpec("dop64", dsd64)}, reason: "dsd_tuple_not_approved"},
		{name: "auto_native_drift", mode: "auto", allow: true, specs: []policySpec{
			constrained(nativeSpec("n64", "DSD_U32_BE", dsd64), "Native Gate", drifted),
			constrained(dopSpec("dop64", dsd64), "DoP Gate", drifted),
		}, live: listControls{gateControl("Native Gate", 2), gateControl("DoP Gate", 1)}, reason: "dsd_policy_unavailable"},
		{name: "dsd512", mode: "auto", allow: true, specs: []policySpec{nativeSpec("n64", "DSD_U32_BE", dsd64), dopSpec("dop64", dsd64)}, content: dsdContent(t, 22579200, 0), reason: ReasonRate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := approvedDSDFixture(t, tc.mode, tc.allow, nil, tc.specs...)
			if tc.live != nil {
				cfg.Authorization.Controls = alsactl.Reader{Open: func(int) (alsactl.Device, error) { return tc.live, nil }}
			}
			content := tc.content
			if content == nil {
				content = dsdContent(t, dsd64, 0)
			}
			hw := &dsdHardware{}
			hw.counted(&cfg)
			cfg.Resolver = edgeContent(content)
			b, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			err = startDSD(t, b, 1, "content")
			if tc.reason == "" {
				native, pcm := hw.opens()
				if len(pcm) != 0 || len(native) != 1 || native[0].Kind != format.KindNativeDSD || native[0].Format != format.DSDU32BE {
					t.Fatalf("auto with a native and a DoP policy at one rate opened %+v and %d PCM sinks, want the native sink alone", native, len(pcm))
				}
				return
			}
			refusedAs(t, err, tc.reason)
			hw.untouched(t)
		})
	}
}

// constrained is s holding the control gate at 1, its reports taken under
// controls.
func constrained(s policySpec, gate string, controls []alsactl.Control) policySpec {
	s.constraints = []alsactl.Constraint{gateConstraint(gate, 1)}
	s.controls = controls
	return s
}
