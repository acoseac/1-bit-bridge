package policy

import (
	"reflect"
	"strings"
	"testing"
)

// mixerTable is a usbmixer file as sound/usb/mixer.c writes it at v6.12
// (snd_usb_mixer_proc_read, snd_usb_mixer_dump_cval): synthetic, two audio
// functions numbering their units alike, a unit with a mute and a volume, an
// element the driver made no control of, and one whose dump says no
// selector.
const mixerTable = `USB Mixer: usb_id=0x25ce0025, ctrlif=0, ctlerr=0
Card: Mytek Brooklyn DAC+ at usb-0000:01:00.0-1.3, high speed
  Unit: 10
    Control: name="PCM Playback Switch", index=0
    Info: id=10, control=1, cmask=0x0, channels=1, type="INV_BOOLEAN"
  Unit: 10
    Control: name="PCM Playback Volume", index=0
    Info: id=10, control=2, cmask=0x0, channels=1, type="S16"
    Volume: min=-32768, max=0, dBmin=-12800, dBmax=0
  Unit: 11
    Info: id=11, control=2, cmask=0x3, channels=2, type="S16"
  Unit: 12
    Control: name="Clock Source 12 Validity", index=0
USB Mixer: usb_id=0x25ce0025, ctrlif=3, ctlerr=0
Card: Mytek Brooklyn DAC+ at usb-0000:01:00.0-1.3, high speed
  Unit: 10
    Control: name="Mic Capture Volume", index=1
    Info: id=10, control=2, cmask=0x0, channels=1, type="S16"
`

// TestTheUSBMixerTableParsesAsTheKernelWritesIt (B728 B7): each element
// with a Control line is one entry, in its block's audio function (ctrlif),
// with the selector its Info line names; an element with no control is left
// out, one whose dump names no selector keeps 0, and the Card and Volume
// lines are passed over.
func TestTheUSBMixerTableParsesAsTheKernelWritesIt(t *testing.T) {
	got, err := ParseUSBMixer([]byte(mixerTable))
	if err != nil {
		t.Fatal(err)
	}
	want := []UnitControl{
		{ControlInterface: ptr(0), Unit: 10, Selector: 1, Name: "PCM Playback Switch"},
		{ControlInterface: ptr(0), Unit: 10, Selector: 2, Name: "PCM Playback Volume"},
		{ControlInterface: ptr(0), Unit: 12, Name: "Clock Source 12 Validity"},
		{ControlInterface: ptr(3), Unit: 10, Selector: 2, Name: "Mic Capture Volume", Index: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed %+v\nwant   %+v", got, want)
	}
}

// TestAUSBMixerTableThatDoesNotReadIsAnError (B728 B7): a line the kernel
// would not write where it stands is an error, never a guess, so the check
// takes the table for unread and fails closed; a line it does not know is
// passed over.
func TestAUSBMixerTableThatDoesNotReadIsAnError(t *testing.T) {
	for name, table := range map[string]string{
		"no block":                  "  Unit: 10\n    Control: name=\"PCM Playback Volume\", index=0\n",
		"a control outside a unit":  "USB Mixer: usb_id=0x1, ctrlif=0, ctlerr=0\n    Control: name=\"x\", index=0\n",
		"a unit that does not read": "USB Mixer: usb_id=0x1, ctrlif=0, ctlerr=0\n  Unit: ten\n",
		"a control with no index":   "USB Mixer: usb_id=0x1, ctrlif=0, ctlerr=0\n  Unit: 10\n    Control: name=\"x\"\n",
		"two controls in one unit":  "USB Mixer: usb_id=0x1, ctrlif=0, ctlerr=0\n  Unit: 10\n    Control: name=\"a\", index=0\n    Control: name=\"b\", index=0\n",
		"empty":                     "",
	} {
		if got, err := ParseUSBMixer([]byte(table)); err == nil {
			t.Errorf("%s: parsed %+v, want an error", name, got)
		}
	}
	got, err := ParseUSBMixer([]byte("USB Mixer: usb_id=0x1, ctrlif=0, ctlerr=0\nA line a later kernel adds: 1\n  Unit: 10\n    Control: name=\"PCM, \\\"main\\\" Volume\", index=0\n    Something: else\n"))
	if err != nil || len(got) != 1 || got[0].Name != `PCM, \"main\" Volume` || got[0].Selector != 0 {
		t.Errorf("a table with lines a later kernel adds: %+v, %v; want the one control, its name whole", got, err)
	}
	block := strings.Replace(mixerTable, "ctrlif=0, ", "", 1)
	got, err = ParseUSBMixer([]byte(block))
	if err != nil || got[0].ControlInterface != nil {
		t.Errorf("a block whose header says no ctrlif: %+v, %v; want its entries with no function", got, err)
	}
}

// TestAParsedTableMapsAStageToItsControls (B728 B7): the table the reader
// parses is the one the gain check maps a stage by: the test DAC's volume
// unit, its mute and volume in the parsed table, passes at 0 dB and fails
// off it, and a table that leaves the volume out is unverifiable.
func TestAParsedTableMapsAStageToItsControls(t *testing.T) {
	table := "USB Mixer: usb_id=0x20b13009, ctrlif=0, ctlerr=0\nCard: test\n" +
		"  Unit: 10\n    Control: name=\"PCM Playback Switch\", index=0\n    Info: id=10, control=1, cmask=0x0, channels=1, type=\"INV_BOOLEAN\"\n" +
		"  Unit: 10\n    Control: name=\"PCM Playback Volume\", index=0\n    Info: id=10, control=2, cmask=0x0, channels=1, type=\"S16\"\n"
	units, err := ParseUSBMixer([]byte(table))
	if err != nil {
		t.Fatal(err)
	}
	m := withVolumeUnit(cleanMixer(), switchControl("PCM Playback Switch"), volume("PCM Playback Volume", 255))
	m.Units = units
	if g := gainAt(m, nil); !g.Unity {
		t.Errorf("the volume at 0 dB through the parsed table: %+v, want unity", g)
	}
	m = withVolumeUnit(cleanMixer(), switchControl("PCM Playback Switch"), volume("PCM Playback Volume", 200))
	m.Units = units
	wantGain(t, "the volume off 0 dB through the parsed table", gainAt(m, nil), ReasonGainNotUnity)
	m.Units = units[:1]
	wantGain(t, "a parsed table with no volume entry", gainAt(m, nil), ReasonGainUnverifiable)
}
