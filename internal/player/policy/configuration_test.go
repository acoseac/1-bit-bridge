package policy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
)

// mytekDescriptors are a Mytek Brooklyn DAC+'s USB descriptors as its box's
// sysfs gave them (testdata/README.md): the device descriptor, then two
// configurations of value 1, byte for byte the same.
func mytekDescriptors(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "mytek-brooklyn-dac-plus.descriptors.bin"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// mytekAltsets are the Mytek's playback altsets, on interface 2, as the
// owner's capture of 2026-11-04 describes them (the DAC's profile): S32_LE to
// 384 kHz, DoP's carriers to DSD128's, and native DSD_U32_BE to DSD256's
// frame rate.
func mytekAltsets() []alsa.Altset {
	return []alsa.Altset{
		{Interface: ptr(2), Altset: ptr(1), Formats: []string{"S32_LE"}, Channels: ptr(2),
			Rates: []int{44100, 48000, 88200, 96000, 176400, 192000, 352800, 384000}, Bits: ptr(32)},
		{Interface: ptr(2), Altset: ptr(2), Formats: []string{"SPECIAL", "DSD_U32_BE"}, Channels: ptr(2),
			Rates: []int{88200, 176400, 352800}, Bits: ptr(32)},
	}
}

// mytekMixerTable is the kernel's table of the controls its USB audio driver
// made of the Mytek's feature unit 4, written as sound/usb/mixer.c writes it
// at v6.12: synthetic, since no capture of the unit's table is kept, its
// names the ones the driver gives a feature unit after a USB streaming input
// terminal.
const mytekMixerTable = `USB Mixer: usb_id=0x25ce0025, ctrlif=0, ctlerr=0
Card: Mytek Brooklyn DAC+ at usb-0000:01:00.0-1.3, high speed
  Unit: 4
    Control: name="PCM Playback Switch", index=0
    Info: id=4, control=1, cmask=0x0, channels=1, type="INV_BOOLEAN"
  Unit: 4
    Control: name="PCM Playback Volume", index=0
    Info: id=4, control=2, cmask=0x0, channels=1, type="S16"
    Volume: min=-32768, max=0, dBmin=-12800, dBmax=0
`

// mytekMixer is what the gain check reads of the Mytek's card: its captured
// descriptors in configuration 1, the kernel's table of unit 4
// (mytekMixerTable), and its controls, the mute on and the master volume at
// volume of 0 to 128, -128 dB to 0 dB.
func mytekMixer(t *testing.T, volume int64) Mixer {
	t.Helper()
	units, err := ParseUSBMixer([]byte(mytekMixerTable))
	if err != nil {
		t.Fatal(err)
	}
	return Mixer{Descriptors: mytekDescriptors(t), Config: 1, ControlsRead: true, UnitsRead: true, Units: units,
		Controls: alsactl.Snapshot{Controls: []alsactl.Control{boolean("PCM Playback Switch", 1),
			integer("PCM Playback Volume", 0, 0, 128, dbMinMax(-12800, 0), volume)}}}
}

// TestTheMyteksCapturedDescriptorsGiveItsDoPPath (B773): a Mytek Brooklyn
// DAC+'s descriptors, as its box's sysfs gave them, hold two configurations
// of value 1, byte for byte the same, which the parse took for two and
// refused, so the gain check refused every DoP rate as unverifiable. They
// are one configuration: the playback path of interface 2, at either
// altset, is input terminal 3 through feature unit 4, a master volume, to
// output terminal 5; with that volume at 0 dB the gain check passes and the
// policy plays DoP at DSD64 and DSD128, and off 0 dB it does not. The
// capture holds no string descriptor, so no serial.
func TestTheMyteksCapturedDescriptorsGiveItsDoPPath(t *testing.T) {
	desc := mytekDescriptors(t)
	if len(desc) != 970 || desc[17] != 2 {
		t.Fatalf("the capture: %d bytes, %d configurations; want 970 and 2", len(desc), desc[17])
	}
	first, second := desc[18:18+476], desc[18+476:]
	if !bytes.Equal(first, second) || binary.LittleEndian.Uint16(first[2:]) != 476 || first[5] != 1 {
		t.Fatal("the capture's configurations are not two copies of configuration 1, 476 bytes each")
	}
	for off := 0; off < len(desc); off += int(desc[off]) {
		if desc[off] < 2 || off+int(desc[off]) > len(desc) {
			t.Fatalf("the capture's descriptor at byte %d does not read", off)
		}
		if desc[off+1] == 0x03 {
			t.Fatalf("the capture holds a string descriptor at byte %d", off)
		}
	}
	want := Path{Protocol: 2, Terminal: 3, Outputs: []int{5},
		Units: []Unit{{ID: 4, Kind: UnitFeature, Controls: []FeatureControl{{Selector: selVolume, Master: true}}}}}
	for _, altset := range []int{1, 2} {
		p, err := ParsePlayback(desc, Selection{Config: 1, Interface: 2, Altset: altset})
		if err != nil || !reflect.DeepEqual(p, want) {
			t.Errorf("interface 2, altset %d: %+v, %v; want %+v", altset, p, err, want)
		}
	}
	carriers := DoPAltsets(mytekAltsets(), dsd64)
	wantGain(t, "the Mytek's volume at 0 dB", CheckGain(mytekMixer(t, 128), carriers, nil, "0100"), "")
	wantGain(t, "the Mytek's volume off 0 dB", CheckGain(mytekMixer(t, 64), carriers, nil, "0100"), ReasonGainNotUnity)
	r := newRig(t).everything(dsdstate.ModeDoP)
	r.altsets, r.mixer = mytekAltsets(), mytekMixer(t, 128)
	wantOutcome(t, "DoP at DSD64 on the Mytek at 0 dB", r.plan(dsd64), OutcomeDoP)
	wantOutcome(t, "DoP at DSD128 on the Mytek at 0 dB", r.plan(dsd128), OutcomeDoP)
	r.mixer = mytekMixer(t, 64)
	wantOutcome(t, "DoP at DSD64 on the Mytek off 0 dB", r.plan(dsd64), OutcomeRefused, ReasonGainNotUnity)
}

// withConfigurations is a unit's descriptors holding descs' configurations in
// turn, after the first's device descriptor, its bNumConfigurations their
// count.
func withConfigurations(descs ...[]byte) []byte {
	out := slices.Clone(descs[0][:18])
	out[17] = byte(len(descs))
	for _, d := range descs {
		out = append(out, d[18:]...)
	}
	return out
}

// withConfigByte is desc with its configuration descriptor's byte at off set
// to v: its value at 5, its maximum power at 8.
func withConfigByte(desc []byte, off int, v byte) []byte {
	d := slices.Clone(desc)
	d[18+off] = v
	return d
}

// TestByteIdenticalConfigurationsAreOne (B773): configurations that hold the
// value the unit's interfaces carry are one when they are the same bytes, the
// whole of each by its wTotalLength, however many and whatever lies between
// them, and the path is the one configuration's; two that differ anywhere
// (their bodies under one header, their lengths, their configuration
// descriptors alone) are refused with the word a duplicate always had, and
// the gain check refuses DoP as unverifiable; another value's duplicates are
// not the selected configuration's; and a configuration cut short is refused
// as before, whichever copy it is.
func TestByteIdenticalConfigurationsAreOne(t *testing.T) {
	const (
		dupOf1   = "the descriptors do not resolve: two configurations hold the value 1"
		dupOf2   = "the descriptors do not resolve: two configurations hold the value 2"
		cutShort = "the descriptors do not resolve: a configuration's total length runs past its descriptors"
		missing  = "the descriptors do not resolve: a configuration descriptor is cut short or missing"
	)
	staged := dac2(uac2IT(2, usbStreaming), uac2FU(10, 2, 0x0f, 0, 0), uac2OT(20, speaker, 10))
	// muted is staged's header over another body: its feature unit a mute
	// alone, its length the same.
	muted := dac2(uac2IT(2, usbStreaming), uac2FU(10, 2, 0x03, 0, 0), uac2OT(20, speaker, 10))
	longer := dac2(uac2IT(2, usbStreaming), uac2FU(10, 2, 0x0f, 0, 0), uac2FU(11, 10, 0x0f, 0, 0), uac2OT(20, speaker, 11))
	if !bytes.Equal(muted[:27], staged[:27]) || bytes.Equal(muted, staged) {
		t.Fatal("the muted configuration is not staged's header over another body")
	}
	two := withConfigurations(staged, staged)
	others := withConfigurations(staged, withConfigByte(muted, 5, 2), withConfigByte(longer, 5, 2))
	path := parse(t, staged)
	for _, c := range []struct {
		name  string
		desc  []byte
		value int
		// refused is the parse's refusal, "" where the path resolves.
		refused string
	}{
		{"one configuration", staged, 1, ""},
		{"two the same", two, 1, ""},
		{"three the same", withConfigurations(staged, staged, staged), 1, ""},
		{"two the same, another value's between them", withConfigurations(staged, withConfigByte(muted, 5, 2), staged), 1, ""},
		{"two under one header, their bodies differing", withConfigurations(muted, staged), 1, dupOf1},
		{"two under one header, their bodies differing, the other way", withConfigurations(staged, muted), 1, dupOf1},
		{"two of different lengths", withConfigurations(staged, longer), 1, dupOf1},
		{"two differing in their configuration descriptors alone", withConfigurations(staged, withConfigByte(staged, 8, 100)), 1, dupOf1},
		{"three, the third differing", withConfigurations(staged, staged, muted), 1, dupOf1},
		{"another value's two, differing", others, 1, ""},
		{"the value of the two differing", others, 2, dupOf2},
		{"two the same, the second cut short", two[:len(two)-1], 1, cutShort},
		{"two the same, the first cut short", withConfigurations(staged[:len(staged)-1], staged), 1, missing},
	} {
		p, err := ParsePlayback(c.desc, Selection{Config: c.value, Interface: 1, Altset: 1})
		m := withVolumeUnit(cleanMixer(), volume("PCM Playback Volume", 255), switchControl("PCM Playback Switch"))
		m.Descriptors, m.Config = c.desc, c.value
		g := CheckGain(m, []alsa.Altset{s32Altset()}, nil, bcd)
		if c.refused == "" {
			if err != nil || !reflect.DeepEqual(p, path) {
				t.Errorf("%s: %+v, %v; want the one configuration's path %+v", c.name, p, err, path)
			}
			wantGain(t, c.name+", its volume at 0 dB", g, "")
			continue
		}
		if !errors.Is(err, errTopology) || err.Error() != c.refused {
			t.Errorf("%s: %+v, %v; want %q", c.name, p, err, c.refused)
		}
		wantGain(t, c.name+", its volume at 0 dB", g, ReasonGainUnverifiable)
	}
	// The Mytek's own: its second copy cut short by a byte, and its second
	// copy's feature unit 4 a mute alone.
	mytek := mytekDescriptors(t)
	mytekAt := func(desc []byte) error {
		_, err := ParsePlayback(desc, Selection{Config: 1, Interface: 2, Altset: 1})
		return err
	}
	if err := mytekAt(mytek[:len(mytek)-1]); !errors.Is(err, errTopology) || err.Error() != cutShort {
		t.Errorf("the Mytek's, its second copy cut short: %v; want %q", err, cutShort)
	}
	changed := slices.Clone(mytek)
	const unit4Master = 18 + 476 + 106 + 5
	if changed[unit4Master] != 0x0f {
		t.Fatalf("byte %d of the Mytek's is %#x, not its second copy's feature unit 4's master controls", unit4Master, changed[unit4Master])
	}
	changed[unit4Master] = 0x03
	if err := mytekAt(changed); !errors.Is(err, errTopology) || err.Error() != dupOf1 {
		t.Errorf("the Mytek's, its second copy's feature unit a mute alone: %v; want %q", err, dupOf1)
	}
}
