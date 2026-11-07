package policy

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
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
