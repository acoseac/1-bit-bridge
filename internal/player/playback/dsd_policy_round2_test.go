package playback_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// Round 2 of B728 B7's review, without a device: the model the set publishes,
// and the DoP gain check binding DoP alone.

// TestThePublishedModelNamesNoSerialOrPort (B728 B7 round 2): the set's model
// is the unit's manufacturer and product strings as the reading gives them,
// every occurrence of the unit's own serial and port path in them replaced
// with [redacted] (PROTOCOL.md's dsdPlayback.model), whatever the binding
// names: one by model or by port names neither, so a product string that
// carries the serial, and a manufacturer string that carries the port, are
// published without them.
func TestThePublishedModelNamesNoSerialOrPort(t *testing.T) {
	box := playbacktest.NewBox(5)
	unit := playbacktest.DACUnit()
	// The test DAC's serial is TESTDAC-0001 (playbacktest.DACUSB).
	unit.Manufacturer = "Test at " + unit.USB.Port
	unit.Product = playbacktest.DACProductName + " TESTDAC-0001"
	box.SetUnit(5, unit)
	b := policyBackend(t, box, playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative)))
	b.RefreshDSD(true)
	p := b.Policy()
	if strings.Contains(p.Model, "TESTDAC-0001") || strings.Contains(p.Model, unit.USB.Port) || p.Model != "Test at [redacted] Test DAC [redacted]" {
		t.Errorf("the set publishes the model %q, want the unit's strings with its serial and its port path redacted", p.Model)
	}
}

// TestTheProbesCheckRefusesAnEnumerationThatDoesNotRead (B728 B7 round 3):
// the probe's check of the node it opened requires the unit's device number,
// read and positive, to be the one it captured: a device number that did not
// read (0) tells no enumeration from another, so two of them are no
// enumeration in common, and the node is refused as another unit's.
func TestTheProbesCheckRefusesAnEnumerationThatDoesNotRead(t *testing.T) {
	box := playbacktest.NewBox(5)
	unit := playbacktest.DACUnit()
	unit.USB.DevNum = 0
	box.SetUnit(5, unit)
	node := alsapcm.NodeInfo{Card: 5, Playback: true}
	if err := playback.ProbedUnitForTest(box, unit, playbacktest.DACKey())(node); alsapcm.ReasonOf(err) != alsapcm.ReasonWrongDevice {
		t.Errorf("the check of a node whose unit's device number did not read: %v, want it refused as another unit", err)
	}
	unit.USB.DevNum = 2
	box.SetUnit(5, unit)
	if err := playback.ProbedUnitForTest(box, unit, playbacktest.DACKey())(node); err != nil {
		t.Errorf("the check of the unit captured, its device number read: %v", err)
	}
}

// TestAControlIndexPastAUint32LeavesDoPUnverifiable (B728 B7 round 3; CodeQL):
// a control index in the kernel's table past what a control's index holds (a
// uint32) does not read, so the table is taken for unread and DoP through the
// gain stage is refused as unverifiable, failing closed: read with no bound,
// 2^32 wrapped to index 0, named the volume control at 0 dB, and DoP played.
func TestAControlIndexPastAUint32LeavesDoPUnverifiable(t *testing.T) {
	box := playbacktest.NewBox(5)
	unit := playbacktest.DACUnit()
	unit.Descriptors = playbacktest.VolumeDescriptors()
	box.SetUnit(5, unit)
	box.SetControls(5, playbacktest.VolumeControls(255)...)
	table := strings.Replace(string(playbacktest.VolumeMixer()), `name="PCM Playback Volume", index=0`, `name="PCM Playback Volume", index=4294967296`, 1)
	box.SetMixer(5, []byte(table))
	b := policyBackend(t, box, playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeDoP)))
	if plan, _, err := b.AdmitDSDForTest(dsdAt(playbacktest.DSD64), true); reasonOf(err) != "dop_gain_unverifiable" {
		t.Errorf("DoP through a volume whose table index is past a uint32: %+v, %v; want dop_gain_unverifiable", plan, err)
	}
}

// TestANativeRateIsUnaffectedByAnUnverifiableDoPGain (B728 B7 round 2): the
// DoP gain check binds DoP alone: with a feature unit on the playback path and
// the kernel's table of its controls not read, a migrated native rate plays
// native and is published, under auto beside a DoP rate, on a DAC the profile
// database knows and on one it does not, and under native alone. Under auto
// the DoP rate whose gain cannot be verified converts, as the design's section
// 3.2 has auto do wherever neither transport qualifies, which the player does
// since B728's B11; only an explicit dop names dop_gain_unverifiable
// (TestAMigratedDoPSetupWithUnverifiableGainRefuses).
func TestANativeRateIsUnaffectedByAnUnverifiableDoPGain(t *testing.T) {
	mixed := playbacktest.MigratedState(dsdstate.ModeAuto, true, []int{playbacktest.DSD64}, []int{playbacktest.DSD128})
	for _, c := range []struct {
		name      string
		st        dsdstate.State
		profiles  *dacprofile.Database
		native    []int
		converted []int
	}{
		{"a migrated mixed setup, a DAC the database knows", mixed, playbacktest.KnownProfiles(nil), []int{playbacktest.DSD64}, []int{playbacktest.DSD128}},
		{"a migrated mixed setup, a DAC the database does not know", mixed, nil, []int{playbacktest.DSD64}, []int{playbacktest.DSD128}},
		{"a migrated native setup", playbacktest.MigratedState(dsdstate.ModeNative, false, []int{playbacktest.DSD64, playbacktest.DSD128}, nil), nil,
			[]int{playbacktest.DSD64, playbacktest.DSD128}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			box := playbacktest.NewBox(5)
			unit := playbacktest.DACUnit()
			unit.Descriptors = playbacktest.VolumeDescriptors()
			box.SetUnit(5, unit)
			box.SetControls(5, playbacktest.VolumeControls(255)...)
			box.SetMixer(5, []byte("this is not the kernel's table"))
			b := policyBackend(t, box, playbacktest.NewMemStore(c.st), func(p *playback.DSDPolicy) { p.Profiles = c.profiles })
			for _, rate := range c.native {
				if plan, _, err := b.AdmitDSDForTest(dsdAt(rate), true); err != nil || plan.Tuple.Transport != "" {
					t.Errorf("DSD at %d with DoP's gain unverifiable: %+v, %v; want it native", rate, plan, err)
				}
			}
			for _, rate := range c.converted {
				if _, _, converts, err := b.AdmitDSDPlanForTest(dsdAt(rate), true); err != nil || !converts {
					t.Errorf("DoP at %d with its gain unverifiable under auto: converts %v, %v; want it converted", rate, converts, err)
				}
			}
			p := b.Policy()
			if rates, transports := ratesOf(p); !slices.Equal(rates, c.native) || slices.ContainsFunc(transports, func(s string) bool { return s != "" }) || p.Reason != "" {
				t.Errorf("the set publishes %v (%q, reason %q), want native at %v alone", rates, transports, p.Reason, c.native)
			}
		})
	}
}
