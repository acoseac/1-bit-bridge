package policy

import (
	"fmt"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// wantGain fails t unless g is unity, or not unity with reason.
func wantGain(t *testing.T, what string, g Gain, reason engine.Reason) {
	t.Helper()
	switch {
	case reason == "" && !g.Unity:
		t.Errorf("%s: %s (%s), want unity", what, g.Reason, g.Detail)
	case reason != "" && (g.Unity || g.Reason != reason):
		t.Errorf("%s: unity %v, %s (%s), want %s", what, g.Unity, g.Reason, g.Detail, reason)
	}
}

// analogueProfile is a known DAC whose profile names an analogue control
// after its DoP decoder and a raw-maximum control on firmware 0100.
func analogueProfile() *dacprofile.Profile {
	return &dacprofile.Profile{ID: "test.dac", DoP: &dacprofile.DoP{
		HeardMaxBitRate:      dsd256,
		AnalogVolumeControls: []dacprofile.AnalogControl{{ControlID: dacprofile.ControlID{Interface: "MIXER", Name: "Master Playback Volume", Index: 0}}},
		UnityAtRawMax: []dacprofile.RawMaxControl{{ControlID: dacprofile.ControlID{Interface: "MIXER", Name: "Digital Playback Volume", Index: 0},
			RawMax: 255, BCDDevice: []string{"0100"}, Evidence: dacprofile.Evidence{Kind: "document", Title: "Manual", Section: "4.2"}}},
	}}
}

// TestAMissingControlIsNotUnity: a feature unit on the playback path with a
// gain control and no visible level control of it fails the check as
// unverifiable, however the controls the card does show read: no control at
// all, its mute switch alone, a control the kernel's table names that the
// card does not hold, a table that did not read, a gain control on the
// channels the kernel made no control of, two gain controls with one
// control shown. Its control at 0 dB passes.
func TestAMissingControlIsNotUnity(t *testing.T) {
	m := withVolumeUnit(cleanMixer(), volume("PCM Playback Volume", 255), switchControl("PCM Playback Switch"))
	wantGain(t, "the unit's volume visible at 0 dB", gainAt(m, nil), "")
	cases := map[string]func(*Mixer){
		"no control at all":     func(m *Mixer) { m.Controls.Controls, m.Units = nil, nil },
		"the mute switch alone": func(m *Mixer) { m.Controls.Controls = []alsactl.Control{switchControl("PCM Playback Switch")} },
		"the table's control is not the card's": func(m *Mixer) {
			m.Controls.Controls = []alsactl.Control{integer("PCM Playback Volume", 1, 0, 255, dbMinMaxMute(-12700, 0), 255, 255), switchControl("PCM Playback Switch")}
		},
		"the table names none of the unit's": func(m *Mixer) { m.Units = []UnitControl{{Unit: 11, Name: "PCM Playback Volume"}} },
		"the table did not read":             func(m *Mixer) { m.Units, m.UnitsRead = nil, false },
		"a volume on the channels, none made of it": func(m *Mixer) {
			m.Descriptors = dac2(uac2IT(2, usbStreaming), uac2FU(10, 2, 0x0f, 0, 0x0c), uac2OT(20, speaker, 10))
		},
		"a volume and a bass, one control shown": func(m *Mixer) {
			m.Descriptors = dac2(uac2IT(2, usbStreaming), uac2FU(10, 2, 0x0f|0x30, 0, 0), uac2OT(20, speaker, 10))
		},
	}
	for name, mutate := range cases {
		m := withVolumeUnit(cleanMixer(), volume("PCM Playback Volume", 255), switchControl("PCM Playback Switch"))
		mutate(&m)
		wantGain(t, name, gainAt(m, nil), ReasonGainUnverifiable)
	}
	// The policy refuses DoP over it.
	r := newRig(t).everything("dop").withoutNative()
	r.mixer = withVolumeUnit(r.mixer, switchControl("PCM Playback Switch"))
	wantOutcome(t, "DoP beside a volume with no visible control", r.plan(testRate), OutcomeRefused, ReasonGainUnverifiable)
}

// TestEveryLevelControlCountsWhateverItsName: on a playback path with no gain
// stage, every MIXER integer control on the card is a level control, whatever
// its name says, and one off 0 dB refuses; one the kernel's table makes of a
// gain stage on the path counts whatever its name, a capture's included.
func TestEveryLevelControlCountsWhateverItsName(t *testing.T) {
	for _, name := range []string{"Master Playback Volume", "PCM Playback Volume", "PCM", "Digital Gain", "Feature 10", "Headphone Volume", "Capture Card Playback Volume", "Input Gain Control"} {
		m := cleanMixer()
		m.Controls.Controls = []alsactl.Control{volume(name, 200)}
		wantGain(t, name+" off 0 dB", gainAt(m, nil), ReasonGainNotUnity)
		m.Controls.Controls = []alsactl.Control{volume(name, 255)}
		wantGain(t, name+" at 0 dB", gainAt(m, nil), "")
	}
	// An integer control with no dB scale counts as well: it cannot be read
	// as unity.
	m := cleanMixer()
	m.Controls.Controls = []alsactl.Control{integer("Tone Control - Bass", 0, -48, 48, nil, 0, 0)}
	wantGain(t, "a bass control with no dB scale", gainAt(m, nil), ReasonGainUnverifiable)
	// A 64-bit integer control too.
	wide := volume("Master Volume", 200)
	wide.Type = typeInteger64
	m.Controls.Controls = []alsactl.Control{wide}
	wantGain(t, "a 64-bit integer control off 0 dB", gainAt(m, nil), ReasonGainNotUnity)
	// A control the table makes of the path's gain stage counts, though its
	// name marks a capture.
	m = withVolumeUnit(cleanMixer(), volume("PCM Capture Volume", 200), switchControl("PCM Playback Switch"))
	m.Units = []UnitControl{{Unit: 10, Name: "PCM Capture Volume"}}
	wantGain(t, "the path's control named as a capture's, off 0 dB", gainAt(m, nil), ReasonGainNotUnity)
	// Switches and enumerations are not gains.
	m = cleanMixer()
	enum := alsactl.Control{Interface: ifaceMixer, Name: "Clock Source", Type: typeEnumerated, Access: 1, Count: 1, Values: make([]byte, 4), Info: make([]byte, 128)}
	m.Controls.Controls = []alsactl.Control{switchControl("PCM Playback Switch"), enum}
	wantGain(t, "a switch and an enumeration", gainAt(m, nil), "")
}

// TestACaptureControlIsExcludedByItsName: a control whose name marks it a
// capture control, its direction (the word before its last) Capture, is not
// a level control, readable or not; a name that only mentions a capture, or
// spells the direction otherwise, counts.
func TestACaptureControlIsExcludedByItsName(t *testing.T) {
	for _, name := range []string{"Mic Capture Volume", "Capture Volume", "Line Bypass Capture Volume", "PCM Capture Gain"} {
		m := cleanMixer()
		m.Controls.Controls = []alsactl.Control{volume(name, 0)}
		wantGain(t, name+" off 0 dB", gainAt(m, nil), "")
		m.Controls.Controls = []alsactl.Control{unreadable(name, 0)}
		wantGain(t, name+", unreadable", gainAt(m, nil), "")
	}
	for _, name := range []string{"capture volume", "Mic capture Volume", "Mic Capture  Volume", "CaptureVolume", "Capture", "Capture Card Playback Volume", "Mic Capture Volume "} {
		m := cleanMixer()
		m.Controls.Controls = []alsactl.Control{volume(name, 0)}
		wantGain(t, name+" off 0 dB", gainAt(m, nil), ReasonGainNotUnity)
	}
}

// TestTheAnalogueListAndTheRawMaxFlagAreTheOnlyExemptions: on a known DAC, a
// level control its profile names after its DoP decoder is passed over
// whatever it reads, and a raw-maximum control passes at its raw maximum;
// nothing else is exempt: the same controls on an unknown DAC refuse, and
// every other level control must read 0 dB.
func TestTheAnalogueListAndTheRawMaxFlagAreTheOnlyExemptions(t *testing.T) {
	controls := []alsactl.Control{
		volume("Master Playback Volume", 100),
		integer("Digital Playback Volume", 0, 0, 255, nil, 255, 255),
		volume("PCM Playback Volume", 255),
	}
	m := cleanMixer()
	m.Controls.Controls = controls
	wantGain(t, "the profile's exemptions", gainAt(m, analogueProfile()), "")
	wantGain(t, "the same controls on an unknown DAC", gainAt(m, nil), ReasonGainUnverifiable)
	only := cleanMixer()
	only.Controls.Controls = []alsactl.Control{volume("Master Playback Volume", 100)}
	wantGain(t, "the analogue control on an unknown DAC", gainAt(only, nil), ReasonGainNotUnity)
	// Another level control off 0 dB refuses beside them.
	m.Controls.Controls = append(append([]alsactl.Control(nil), controls...), volume("Headphone Playback Volume", 200))
	wantGain(t, "another level control off 0 dB", gainAt(m, analogueProfile()), ReasonGainNotUnity)
	// A profile with no DoP claim exempts nothing.
	m.Controls.Controls = controls
	wantGain(t, "a profile with no DoP section", gainAt(m, &dacprofile.Profile{ID: "test.dac"}), ReasonGainUnverifiable)
}

// TestAnExemptionMatchesItsControlExactly: a profile's control is matched by
// its interface, its name and its index exactly, at device and subdevice 0:
// another index, another case, a trailing space, another interface or
// another device is another control, and not exempt.
func TestAnExemptionMatchesItsControlExactly(t *testing.T) {
	near := []alsactl.Control{
		integer("Master Playback Volume", 1, 0, 255, dbMinMaxMute(-12700, 0), 100, 100),
		volume("master Playback Volume", 100),
		volume("Master Playback Volume ", 100),
		volume("Master Playback", 100),
	}
	for _, c := range near {
		m := cleanMixer()
		m.Controls.Controls = []alsactl.Control{c}
		wantGain(t, fmt.Sprintf("%q, index %d", c.Name, c.Index), gainAt(m, analogueProfile()), ReasonGainNotUnity)
	}
	dev := volume("Master Playback Volume", 100)
	dev.Device = 1
	m := cleanMixer()
	m.Controls.Controls = []alsactl.Control{dev}
	wantGain(t, "the analogue control's name on device 1", gainAt(m, analogueProfile()), ReasonGainNotUnity)
	p := analogueProfile()
	p.DoP.AnalogVolumeControls[0].Interface = "CARD"
	m.Controls.Controls = []alsactl.Control{volume("Master Playback Volume", 100)}
	wantGain(t, "an entry on the CARD interface", gainAt(m, p), ReasonGainNotUnity)
	raw := integer("Digital Playback Volume", 1, 0, 255, nil, 255, 255)
	m.Controls.Controls = []alsactl.Control{raw}
	wantGain(t, "the raw-maximum control's name at index 1", gainAt(m, analogueProfile()), ReasonGainUnverifiable)
}

// TestARawMaxControlPassesOnlyAtItsRawMaximum: a raw-maximum control passes
// only reading exactly its rawMax on every channel, its range's maximum
// still rawMax, on a firmware its entry lists: below it is not unity, a
// changed range, an unlisted firmware or an unreadable control is
// unverifiable.
func TestARawMaxControlPassesOnlyAtItsRawMaximum(t *testing.T) {
	check := func(what string, c alsactl.Control, firmware string, reason engine.Reason) {
		t.Helper()
		m := cleanMixer()
		m.Controls.Controls = []alsactl.Control{c}
		p := analogueProfile()
		p.DoP.UnityAtRawMax[0].BCDDevice = []string{"0100", "01ab"}
		wantGain(t, what, CheckGain(m, []alsa.Altset{s32Altset()}, p, firmware), reason)
	}
	check("at its raw maximum", integer("Digital Playback Volume", 0, 0, 255, nil, 255, 255), bcd, "")
	check("one below it", integer("Digital Playback Volume", 0, 0, 255, nil, 254, 254), bcd, ReasonGainNotUnity)
	check("below it on one channel", integer("Digital Playback Volume", 0, 0, 255, nil, 255, 0), bcd, ReasonGainNotUnity)
	check("its range's maximum changed, at the new maximum", integer("Digital Playback Volume", 0, 0, 511, nil, 511, 511), bcd, ReasonGainUnverifiable)
	check("its range's maximum changed, at the old one", integer("Digital Playback Volume", 0, 0, 511, nil, 255, 255), bcd, ReasonGainUnverifiable)
	check("on a firmware the entry does not list", integer("Digital Playback Volume", 0, 0, 255, nil, 255, 255), "0200", ReasonGainUnverifiable)
	check("on a firmware that did not read", integer("Digital Playback Volume", 0, 0, 255, nil, 255, 255), "", ReasonGainUnverifiable)
	check("unreadable", unreadable("Digital Playback Volume", 0), bcd, ReasonGainUnverifiable)
	check("a firmware the entry lists, read in upper case", integer("Digital Playback Volume", 0, 0, 255, nil, 255, 255), "01AB", "")
}

// TestAnUnreadableControlFailsTheCheck: a MIXER control that could not be
// read (its identity kept, its type unknown) is a level control before any
// type is asked of it, and fails the check beside a readable control at
// unity; its identity still excludes a capture control and exempts a listed
// analogue one, and one on another interface is no level control.
func TestAnUnreadableControlFailsTheCheck(t *testing.T) {
	m := cleanMixer()
	m.Controls.Controls = []alsactl.Control{volume("PCM Playback Volume", 255), unreadable("Master Playback Volume", 1)}
	wantGain(t, "an unreadable control beside one at unity", gainAt(m, nil), ReasonGainUnverifiable)
	m.Controls.Controls = []alsactl.Control{volume("PCM Playback Volume", 255), unreadable("Mic Capture Volume", 0)}
	wantGain(t, "an unreadable capture control", gainAt(m, nil), "")
	m.Controls.Controls = []alsactl.Control{volume("PCM Playback Volume", 255), unreadable("Master Playback Volume", 0)}
	wantGain(t, "an unreadable listed analogue control", gainAt(m, analogueProfile()), "")
	pcm := unreadable("IEC958 Playback Default", 0)
	pcm.Interface = 3
	m.Controls.Controls = []alsactl.Control{volume("PCM Playback Volume", 255), pcm}
	wantGain(t, "an unreadable control on the PCM interface", gainAt(m, nil), "")
	// The kernel's table maps an unreadable control to the path's stage: it
	// is visible, and cannot be read as unity.
	m = withVolumeUnit(cleanMixer(), unreadable("PCM Playback Volume", 0), switchControl("PCM Playback Switch"))
	wantGain(t, "the path's control, unreadable", gainAt(m, nil), ReasonGainUnverifiable)
}

// TestADBScaleReadsZeroDBExactly: a level control is at unity only where its
// dB TLV reads exactly 0 dB, in exact arithmetic over each scale alsa-lib
// reads (DB_SCALE, DB_MINMAX and its mute variant, DB_LINEAR, DB_RANGE, one
// in a container): a mute position is not unity, a value just off 0 dB is
// not, a value in no part of a range and a TLV with no dB scale cannot be
// read as unity.
func TestADBScaleReadsZeroDBExactly(t *testing.T) {
	cases := []struct {
		name     string
		min, max int64
		tlv      []byte
		value    int64
		reason   engine.Reason
	}{
		{"DB_MINMAX_MUTE at its maximum of 0 dB", 0, 255, dbMinMaxMute(-12700, 0), 255, ""},
		{"DB_MINMAX_MUTE one step below", 0, 255, dbMinMaxMute(-12700, 0), 254, ReasonGainNotUnity},
		{"DB_MINMAX_MUTE at its mute position", 0, 255, dbMinMaxMute(0, 600), 0, ReasonGainNotUnity},
		{"DB_MINMAX at its minimum of 0 dB", 0, 255, dbMinMax(0, 600), 0, ""},
		{"DB_MINMAX with headroom, exactly at 0 dB", 0, 133, dbMinMax(-12700, 600), 127, ""},
		{"DB_MINMAX with headroom, a step above 0 dB", 0, 133, dbMinMax(-12700, 600), 128, ReasonGainNotUnity},
		{"DB_MINMAX between steps of 0 dB", 0, 1000, dbMinMax(-12700, 600), 955, ReasonGainNotUnity},
		{"DB_SCALE at 0 dB", 0, 100, dbScaleTLV(-9000, 100, false), 90, ""},
		{"DB_SCALE a step below", 0, 100, dbScaleTLV(-9000, 100, false), 89, ReasonGainNotUnity},
		{"DB_SCALE at its mute minimum of 0 dB", 0, 100, dbScaleTLV(0, 100, true), 0, ReasonGainNotUnity},
		{"DB_LINEAR at its maximum of 0 dB", 0, 100, dbLinear(-9999999, 0), 100, ""},
		{"DB_LINEAR below its maximum", 0, 100, dbLinear(-9999999, 0), 99, ReasonGainNotUnity},
		{"DB_LINEAR from mute to +20 dB, at a tenth", 0, 100, dbLinear(-9999999, 2000), 10, ""},
		{"DB_LINEAR from mute to +20 dB, off a tenth", 0, 100, dbLinear(-9999999, 2000), 11, ReasonGainNotUnity},
		{"DB_LINEAR from -20 dB to +20 dB, at unity", 0, 99, dbLinear(-2000, 2000), 9, ""},
		{"DB_LINEAR from -6 dB to +6 dB, which never reads 0 dB between them", 0, 100, dbLinear(-600, 600), 50, ReasonGainNotUnity},
		{"DB_RANGE, the upper part at 0 dB", 0, 200, dbRange(rangePart(0, 99, dbScaleTLV(-9900, 100, true)), rangePart(100, 200, dbMinMax(-100, 0))), 200, ""},
		{"DB_RANGE, the lower part", 0, 200, dbRange(rangePart(0, 99, dbScaleTLV(-9900, 100, true)), rangePart(100, 200, dbMinMax(-100, 0))), 50, ReasonGainNotUnity},
		{"DB_RANGE, a value in no part", 0, 300, dbRange(rangePart(0, 99, dbScaleTLV(-9900, 100, true))), 250, ReasonGainUnverifiable},
		{"a container holding one dB scale", 0, 255, container(tlv(0x101, 3, 4), dbMinMaxMute(-12700, 0)), 255, ""},
		{"a container holding two", 0, 255, container(dbMinMaxMute(-12700, 0), dbMinMax(-12700, 0)), 255, ReasonGainUnverifiable},
		{"a channel map alone", 0, 255, tlv(0x101, 3, 4), 255, ReasonGainUnverifiable},
		{"no TLV", 0, 255, nil, 255, ReasonGainUnverifiable},
		{"a TLV cut short", 0, 255, dbMinMax(-12700, 0)[:10], 255, ReasonGainUnverifiable},
		{"a TLV whose length runs past it", 0, 255, append(tlv(tlvDBMinMax), 1, 2, 3), 255, ReasonGainUnverifiable},
	}
	for _, c := range cases {
		m := cleanMixer()
		m.Controls.Controls = []alsactl.Control{integer("PCM Playback Volume", 0, c.min, c.max, c.tlv, c.value, c.value)}
		wantGain(t, c.name, gainAt(m, nil), c.reason)
	}
}

// TestWhatTheCheckCannotReadIsUnverifiable: descriptors or controls that did
// not read, a topology that does not resolve, an altset whose numbers did
// not read, and a unit on the path that no control shows the effect of
// fail the check as unverifiable, which outranks a level control off 0 dB.
func TestWhatTheCheckCannotReadIsUnverifiable(t *testing.T) {
	cases := map[string]func(*Mixer){
		"the descriptors did not read": func(m *Mixer) { m.Descriptors = nil },
		"the controls did not read":    func(m *Mixer) { m.ControlsRead = false },
		"the topology does not resolve": func(m *Mixer) {
			m.Descriptors = dac2(uac2IT(2, usbStreaming), uac2OT(20, speaker, 9))
		},
		"another configuration": func(m *Mixer) { m.Config = 2 },
		"a mixer unit on the path": func(m *Mixer) {
			m.Descriptors = dac2(uac2IT(2, usbStreaming), []byte{14, 0x24, 0x04, 30, 1, 2, 2, 3, 0, 0, 0, 0, 0, 0}, uac2OT(20, speaker, 30))
		},
	}
	for name, mutate := range cases {
		m := cleanMixer()
		m.Controls.Controls = []alsactl.Control{volume("PCM Playback Volume", 200)}
		mutate(&m)
		wantGain(t, name, gainAt(m, nil), ReasonGainUnverifiable)
	}
	m := cleanMixer()
	a := s32Altset()
	a.Altset = nil
	wantGain(t, "an altset whose setting did not read", CheckGain(m, []alsa.Altset{a}, nil, bcd), ReasonGainUnverifiable)
	wantGain(t, "no altset at all", CheckGain(m, nil, nil, bcd), ReasonGainUnverifiable)
}
