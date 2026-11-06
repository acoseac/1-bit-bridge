package policy

import (
	"errors"
	"reflect"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
)

// parse is the playback path of interface 1, altset 1 of configuration 1.
func parse(t *testing.T, desc []byte) Path {
	t.Helper()
	p, err := ParsePlayback(desc, Selection{Config: 1, Interface: 1, Altset: 1})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// capture2 is an audio class 2 streaming interface's settings that stream
// toward the host, linked to link.
func capture2(number, link byte) [][]byte {
	return [][]byte{ifc(number, 0, 2, 0x20, 0), ifc(number, 1, 2, 0x20, 1), uac2General(link), epIn()}
}

// TestACaptureOnlyFeatureUnitIsNotOnThePlaybackPath: a feature unit between a
// microphone and a USB streaming terminal, and one between the playback
// terminal and a USB streaming terminal (a loopback toward the host), are on
// no playback path: the path is the playback terminal's toward the device's
// outputs alone, and their controls, named as a capture's, are no level
// controls.
func TestACaptureOnlyFeatureUnitIsNotOnThePlaybackPath(t *testing.T) {
	descs := [][]byte{iad(0, 3), ifc(0, 0, 1, 0x20, 0), uac2Header(), uac2Clock(0x29),
		uac2IT(2, usbStreaming), uac2OT(20, speaker, 2),
		uac2IT(3, microphone), uac2FU(11, 3, 0x0f, 0, 0), uac2OT(21, usbStreaming, 11),
		uac2FU(12, 2, 0x0f, 0, 0), uac2OT(22, usbStreaming, 12)}
	descs = append(descs, stream2(1, 2)...)
	descs = append(descs, capture2(2, 21)...)
	desc := usbDescriptors(1, descs...)
	p := parse(t, desc)
	if !reflect.DeepEqual(p.Outputs, []int{20}) || len(p.Units) != 0 || p.Terminal != 2 || p.Protocol != 2 {
		t.Errorf("the playback path: %+v, want terminal 2 straight to output 20", p)
	}
	m := Mixer{Descriptors: desc, Config: 1, ControlsRead: true, UnitsRead: true,
		Controls: alsactl.Snapshot{Controls: []alsactl.Control{volume("Mic Capture Volume", 10), volume("PCM Capture Volume", 10)}},
		Units:    []UnitControl{entry(0, 11, selVolume, "Mic Capture Volume"), entry(0, 12, selVolume, "PCM Capture Volume")}}
	wantGain(t, "capture units' controls off 0 dB", gainAt(m, nil), "")
	// The capture interface is no playback interface.
	if _, err := ParsePlayback(desc, Selection{Config: 1, Interface: 2, Altset: 1}); !errors.Is(err, errTopology) {
		t.Errorf("the capture interface parsed as a playback one: %v", err)
	}
}

// TestEveryPlaybackPathIsFollowed: a playback terminal that feeds two
// outputs through a feature unit each puts both units on its path, and a
// selector that also takes another input puts itself and what follows it
// there; a level control off 0 dB on any of them refuses, whichever output
// the listener uses. A second streaming interface's path is its own.
func TestEveryPlaybackPathIsFollowed(t *testing.T) {
	descs := [][]byte{iad(0, 3), ifc(0, 0, 1, 0x20, 0), uac2Header(), uac2Clock(0x29),
		uac2IT(2, usbStreaming), uac2FU(10, 2, 0x0f, 0, 0), uac2OT(20, headphones, 10),
		uac2FU(11, 2, 0x0f, 0, 0), uac2OT(21, spdif, 11),
		uac2IT(3, 0x0603), {9, 0x24, 0x05, 12, 2, 2, 3, 0, 0}, uac2FU(13, 12, 0x0f, 0, 0), uac2OT(22, speaker, 13),
		uac2IT(4, usbStreaming), uac2OT(24, 0x0304, 4)}
	descs = append(descs, stream2(1, 2)...)
	descs = append(descs, stream2(2, 4)...)
	desc := usbDescriptors(1, descs...)
	p := parse(t, desc)
	var ids []int
	for _, u := range p.Units {
		ids = append(ids, u.ID)
	}
	if !reflect.DeepEqual(ids, []int{10, 11, 12, 13}) || !reflect.DeepEqual(p.Outputs, []int{20, 21, 22}) {
		t.Errorf("the playback path's units %v and outputs %v, want units 10 to 13 and outputs 20 to 22", ids, p.Outputs)
	}
	other, err := ParsePlayback(desc, Selection{Config: 1, Interface: 2, Altset: 2})
	if err != nil || len(other.Units) != 0 || !reflect.DeepEqual(other.Outputs, []int{24}) {
		t.Errorf("the second interface's path: %+v, %v; want terminal 4 straight to output 24", other, err)
	}
	units := []UnitControl{entry(0, 10, selVolume, "Headphone Playback Volume"), entry(0, 11, selVolume, "SPDIF Playback Volume"), entry(0, 13, selVolume, "Speaker Playback Volume")}
	at := func(spdifValue int64) Mixer {
		return Mixer{Descriptors: desc, Config: 1, ControlsRead: true, UnitsRead: true, Units: units,
			Controls: alsactl.Snapshot{Controls: []alsactl.Control{volume("Headphone Playback Volume", 255), volume("SPDIF Playback Volume", spdifValue), volume("Speaker Playback Volume", 255)}}}
	}
	wantGain(t, "every path at 0 dB", gainAt(at(255), nil), "")
	wantGain(t, "the S/PDIF path's unit off 0 dB", gainAt(at(128), nil), ReasonGainNotUnity)
	m := at(255)
	m.Units = units[:2]
	wantGain(t, "the selector's path's unit with no control of it", gainAt(m, nil), ReasonGainUnverifiable)
}

// TestTheParseReadsBothAudioClasses: an audio class 1 function (its header's
// interface list, its one- or two-byte feature unit controls) and an audio
// class 2 one (its interface association, its two-bit controls) give the
// controls their feature units declare that make them stages, on the master
// and on the channels: every one but mute and audio class 2's read-only
// underflow and overflow, the switches among them; and the audio control
// interface the path is in.
func TestTheParseReadsBothAudioClasses(t *testing.T) {
	uac1 := func(fu []byte) []byte {
		return usbDescriptors(1, ifc(0, 0, 1, 0, 0), uac1Header(1), uac1IT(1, usbStreaming), fu, uac1OT(3, speaker, 2),
			ifc(1, 0, 2, 0, 0), ifc(1, 1, 2, 0, 1), uac1General(1), uac1EpOut())
	}
	p := parse(t, uac1(uac1FU(2, 1, 0x03, 0x04, 0x04)))
	want := []Unit{{ID: 2, Kind: UnitFeature, Controls: []FeatureControl{{Selector: 2, Master: true}, {Selector: 3, Channels: true}}}}
	if p.Protocol != 1 || p.ControlInterface != 0 || !reflect.DeepEqual(p.Units, want) {
		t.Errorf("audio class 1: %+v, want %+v", p, want)
	}
	// Automatic gain (0x40), delay (0x80), bass boost (0x100) and loudness
	// (0x200) take two bytes a channel.
	p = parse(t, uac1(uac1FU2(2, 1, 0x01|0x40|0x80|0x100|0x200, 0, 0)))
	want = []Unit{{ID: 2, Kind: UnitFeature, Controls: []FeatureControl{{Selector: 7, Master: true}, {Selector: 8, Master: true}, {Selector: 9, Master: true}, {Selector: 10, Master: true}}}}
	if !reflect.DeepEqual(p.Units, want) {
		t.Errorf("audio class 1's switches and delay: %+v, want %+v", p.Units, want)
	}
	p = parse(t, dac2(uac2IT(2, usbStreaming), uac2FU(10, 2, 0x0f, 0x3<<20, 0x1<<22), uac2OT(20, speaker, 10)))
	want = []Unit{{ID: 10, Kind: UnitFeature, Controls: []FeatureControl{{Selector: 2, Master: true}, {Selector: 11, Channels: true}, {Selector: 12, Channels: true}}}}
	if p.Protocol != 2 || p.ControlInterface != 0 || !reflect.DeepEqual(p.Units, want) {
		t.Errorf("audio class 2: %+v, want %+v", p, want)
	}
	// Automatic gain (bits 13-12), delay (15-14), bass boost (17-16),
	// loudness (19-18) and the phase inverter (25-24), on the master and on
	// a channel.
	p = parse(t, dac2(uac2IT(2, usbStreaming), uac2FU(10, 2, 0x3<<12|0x3<<14|0x1<<16, 0x3<<18, 0x3<<24), uac2OT(20, speaker, 10)))
	want = []Unit{{ID: 10, Kind: UnitFeature, Controls: []FeatureControl{{Selector: 7, Master: true}, {Selector: 8, Master: true}, {Selector: 9, Master: true}, {Selector: 10, Channels: true}, {Selector: 13, Channels: true}}}}
	if !reflect.DeepEqual(p.Units, want) {
		t.Errorf("audio class 2's switches and delay: %+v, want %+v", p.Units, want)
	}
	for _, c := range want[0].Controls {
		if c.Switch() != (c.Selector != 8) {
			t.Errorf("selector %d: a switch %v", c.Selector, c.Switch())
		}
	}
	// A feature unit with a mute alone, and audio class 2's read-only
	// underflow (bits 27-26) and overflow (29-28) beside it, declares no
	// stage.
	p = parse(t, dac2(uac2IT(2, usbStreaming), uac2FU(10, 2, 0x03|0x1<<26|0x1<<28, 0x03, 0x03), uac2OT(20, speaker, 10)))
	if len(p.Units) != 1 || len(p.Units[0].Controls) != 0 {
		t.Errorf("a mute alone: %+v, want a feature unit with no stage control", p.Units)
	}
}

// twoFunctions is a unit with two audio class 2 functions, each a control
// interface and a streaming interface, numbering their entities alike, so
// each has a feature unit 10: the first's on its speaker's path (control
// interface 0, streaming interface 1), the second's on its headphones'
// (control interface 2, streaming interface 3).
func twoFunctions() []byte {
	fn := func(control byte, out uint16) [][]byte {
		return [][]byte{iad(control, 2), ifc(control, 0, 1, 0x20, 0), uac2Header(), uac2Clock(0x29),
			uac2IT(2, usbStreaming), uac2FU(10, 2, 0x0f, 0, 0), uac2OT(20, out, 10)}
	}
	descs := fn(0, speaker)
	descs = append(descs, stream2(1, 2)...)
	descs = append(descs, fn(2, headphones)...)
	descs = append(descs, stream2(3, 2)...)
	return usbDescriptors(1, descs...)
}

// TestAnotherAudioFunctionsControlsDoNotStandForThePaths: entity IDs are
// unique within an audio function alone, so a stage is mapped through the
// kernel's table by its function's block (ctrlif) and its unit's ID
// together: another function's unit of the same ID, its control at 0 dB,
// does not stand for the path's, whose control the table does not show, and
// a table entry that does not say its function is unverifiable.
func TestAnotherAudioFunctionsControlsDoNotStandForThePaths(t *testing.T) {
	desc := twoFunctions()
	for _, c := range []struct{ iface, control int }{{1, 0}, {3, 2}} {
		p, err := ParsePlayback(desc, Selection{Config: 1, Interface: c.iface, Altset: 1})
		if err != nil || p.ControlInterface != c.control || len(p.Units) != 1 || p.Units[0].ID != 10 {
			t.Errorf("interface %d: %+v, %v; want feature unit 10 of control interface %d", c.iface, p, err, c.control)
		}
	}
	mixer := func(units ...UnitControl) Mixer {
		return Mixer{Descriptors: desc, Config: 1, ControlsRead: true, UnitsRead: true, Units: units,
			Controls: alsactl.Snapshot{Controls: []alsactl.Control{volume("Headphone Playback Volume", 255), volume("PCM Playback Volume", 255)}}}
	}
	wantGain(t, "the path's unit's control at 0 dB in its function's block", gainAt(mixer(entry(0, 10, selVolume, "PCM Playback Volume"), entry(2, 10, selVolume, "Headphone Playback Volume")), nil), "")
	wantGain(t, "the other function's unit 10 alone in the table", gainAt(mixer(entry(2, 10, selVolume, "Headphone Playback Volume")), nil), ReasonGainUnverifiable)
	unsaid := UnitControl{Unit: 10, Selector: selVolume, Name: "PCM Playback Volume"}
	wantGain(t, "an entry that does not say its function", gainAt(mixer(unsaid), nil), ReasonGainUnverifiable)
}

// TestAnUnresolvedTopologyIsRefused: descriptors the parse cannot follow are
// refused, never taken for a path with no gain stage, and the gain check
// refuses DoP over them as unverifiable.
func TestAnUnresolvedTopologyIsRefused(t *testing.T) {
	it, ot := uac2IT(2, usbStreaming), uac2OT(20, speaker, 2)
	cases := map[string][]byte{
		"no device descriptor":            dac2(it, ot)[18:],
		"a configuration cut short":       dac2(it, ot)[:len(dac2(it, ot))-3],
		"a descriptor past its end":       usbDescriptors(1, ifc(0, 0, 1, 0x20, 0), []byte{40, 0x24}),
		"another configuration":           usbDescriptors(2, iad(0, 2), ifc(0, 0, 1, 0x20, 0), uac2Header(), it, ot),
		"no streaming interface":          usbDescriptors(1, iad(0, 2), ifc(0, 0, 1, 0x20, 0), uac2Header(), it, ot),
		"a link to no entity":             dac2(uac2IT(3, usbStreaming), uac2OT(20, speaker, 3)),
		"a link to an output":             dac2(uac2IT(3, usbStreaming), uac2OT(2, speaker, 3)),
		"a source that is no entity":      dac2(it, uac2FU(10, 9, 0x0f, 0, 0), uac2OT(20, speaker, 10)),
		"a clock as a source":             dac2(it, uac2OT(20, speaker, 0x29)),
		"an output as a source":           dac2(it, uac2OT(21, speaker, 2), uac2FU(10, 21, 0x0f, 0, 0), uac2OT(20, speaker, 10)),
		"a cycle":                         dac2(it, uac2FU(10, 11, 0x0f, 0, 0), uac2FU(11, 10, 0x0f, 0, 0), uac2OT(20, speaker, 10)),
		"an undefined subtype on the way": dac2(it, []byte{6, 0x24, 0x0e, 30, 2, 0}, uac2OT(20, speaker, 30)),
		"output to the host alone":        dac2(it, uac2OT(20, usbStreaming, 2)),
		"a duplicated ID":                 dac2(it, uac2FU(20, 2, 0x0f, 0, 0), uac2OT(20, speaker, 2)),
		"an ID of 0":                      dac2(it, uac2FU(0, 2, 0x0f, 0, 0), uac2OT(20, speaker, 2)),
		"a feature unit cut short":        dac2(it, []byte{9, 0x24, 0x06, 10, 2, 0x0f, 0, 0, 0}, uac2OT(20, speaker, 10)),
		"no interface association":        usbDescriptors(1, ifc(0, 0, 1, 0x20, 0), uac2Header(), uac2Clock(0x29), it, ot, flatten(stream2(1, 2))),
		"audio class 3": usbDescriptors(1, iad(0, 2), ifc(0, 0, 1, 0x30, 0), uac2Header(), it, ot,
			ifc(1, 0, 2, 0x30, 0), ifc(1, 1, 2, 0x30, 2), uac2General(2), epOut()),
		"a streaming setting with no endpoint toward the device": usbDescriptors(1, iad(0, 2), ifc(0, 0, 1, 0x20, 0), uac2Header(), uac2Clock(0x29), it, ot,
			ifc(1, 0, 2, 0x20, 0), ifc(1, 1, 2, 0x20, 1), uac2General(2), epIn()),
		"a streaming setting with no general descriptor": usbDescriptors(1, iad(0, 2), ifc(0, 0, 1, 0x20, 0), uac2Header(), uac2Clock(0x29), it, ot,
			ifc(1, 0, 2, 0x20, 0), ifc(1, 1, 2, 0x20, 2), epOut(), epFeedback()),
		"two control interfaces hold it": usbDescriptors(1, iad(0, 3), ifc(0, 0, 1, 0x20, 0), uac2Header(), uac2Clock(0x29), it, ot,
			ifc(2, 0, 1, 0x20, 0), uac2Header(), flatten(stream2(1, 2))),
		"an audio class 1 header that lists another interface": usbDescriptors(1, ifc(0, 0, 1, 0, 0), uac1Header(2),
			uac1IT(1, usbStreaming), uac1OT(3, speaker, 1), ifc(1, 0, 2, 0, 0), ifc(1, 1, 2, 0, 1), uac1General(1), uac1EpOut()),
	}
	for name, desc := range cases {
		if _, err := ParsePlayback(desc, Selection{Config: 1, Interface: 1, Altset: 1}); !errors.Is(err, errTopology) {
			t.Errorf("%s: parsed (%v)", name, err)
		}
		m := Mixer{Descriptors: desc, Config: 1, ControlsRead: true, UnitsRead: true}
		wantGain(t, name, CheckGain(m, []alsa.Altset{s32Altset()}, nil, bcd), ReasonGainUnverifiable)
	}
}

// flatten joins descriptors.
func flatten(descs [][]byte) []byte {
	var out []byte
	for _, d := range descs {
		out = append(out, d...)
	}
	return out
}

// TestAUnitNoControlShowsIsUnverifiable: on the playback path a selector
// passes the samples as they are, and every other unit but a feature unit (a
// mixer, a processing, an extension or an effect unit, a sample rate
// converter) is a stage the check cannot see.
func TestAUnitNoControlShowsIsUnverifiable(t *testing.T) {
	it := uac2IT(2, usbStreaming)
	cases := map[string]struct {
		unit []byte
		kind UnitKind
	}{
		"a mixer":                 {[]byte{14, 0x24, 0x04, 30, 1, 2, 2, 3, 0, 0, 0, 0, 0, 0}, UnitMixer},
		"a processing unit":       {[]byte{17, 0x24, 0x08, 30, 0x01, 0x00, 1, 2, 2, 3, 0, 0, 0, 0, 0, 0, 0}, UnitProcessing},
		"an extension unit":       {[]byte{16, 0x24, 0x09, 30, 0x01, 0x00, 1, 2, 2, 3, 0, 0, 0, 0, 0, 0}, UnitExtension},
		"an effect unit":          {[]byte{16, 0x24, 0x07, 30, 0x01, 0x00, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0}, UnitEffect},
		"a sample rate converter": {[]byte{8, 0x24, 0x0d, 30, 2, 0x29, 0x29, 0}, UnitRateConverter},
		"a selector":              {[]byte{8, 0x24, 0x05, 30, 1, 2, 0, 0}, UnitSelector},
	}
	for name, c := range cases {
		desc := dac2(it, c.unit, uac2OT(20, speaker, 30))
		p := parse(t, desc)
		if len(p.Units) != 1 || p.Units[0].Kind != c.kind {
			t.Errorf("%s: the path's units %+v, want one %s", name, p.Units, c.kind)
		}
		m := Mixer{Descriptors: desc, Config: 1, ControlsRead: true, UnitsRead: true}
		want := ReasonGainUnverifiable
		if c.kind == UnitSelector {
			want = ""
		}
		wantGain(t, name+" on the path", gainAt(m, nil), want)
	}
}
