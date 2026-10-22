package alsa

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestResolveOutputChainOnTheVolumioCapture(t *testing.T) {
	root := filepath.Join("testdata", "volumio-asound-chain")
	cards := volumioCards(t)
	cases := []struct {
		device string
		stages []string
	}{
		// MPD's device on the box: volumio (empty) -> volumioOutput (plug)
		// -> volumioHw (hw, card "Audio").
		{"volumio", []string{"empty", "plug", "hw"}},
		// The default PCM is one more empty in front of it.
		{"default", []string{"empty", "empty", "plug", "hw"}},
		{"volumioOutput", []string{"plug", "hw"}},
		{"volumioHw", []string{"hw"}},
	}
	for _, tc := range cases {
		chain, ok := ResolveOutputChain(root, tc.device, cards)
		if !ok || chain.Card != 2 || chain.Device != 0 || !slices.Equal(chain.Stages, tc.stages) {
			t.Errorf("%s = %+v, %v; want card 2 device 0 %v", tc.device, chain, ok, tc.stages)
		}
	}
	if chain, ok := ResolveOutputChain(root, "pulse", cards); ok {
		t.Errorf("pulse resolved: %+v", chain)
	}
}

func TestResolveOutputChainNamesEachStage(t *testing.T) {
	cards := []Card{{Index: 0, ID: "vc4hdmi0"}, {Index: 2, ID: "Audio"}, {Index: 5, ID: "A20"}}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `
pcm.out {
    type plug
    slave {
        pcm "hwcard"
    }
}
pcm.hwcard {
    type hw
    card "Audio"
    device 1
}
pcm.toHw { type empty
    slave.pcm "hw:Audio,1" }
pcm.toPlughw { type empty
    slave.pcm "plughw:CARD=A20,DEV=0" }
pcm.toFront { type empty
    slave.pcm "front:Audio" }
pcm.throughDmix {
    type dmix
    slave.pcm "hwcard"
}
pcm.throughSoftvol {
    type softvol
    slave.pcm "hwcard"
}
pcm.throughRate {
    type rate
    slave.pcm "hwcard"
}
pcm.throughRoute {
    type route
    slave.pcm "hwcard"
}
pcm.plugOverDmix { type plug
    slave.pcm "throughDmix" }
pcm.a { type empty
    slave.pcm "b" }
pcm.b { type empty
    slave.pcm "a" }
`
	if err := os.WriteFile(filepath.Join(root, "etc", "asound.conf"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved := []struct {
		device string
		card   int
		pcm    int
		stages []string
	}{
		{"hw:2,0", 2, 0, []string{"hw"}},
		{"hw:CARD=Audio,DEV=1", 2, 1, []string{"hw"}},
		{"hw:A20", 5, 0, []string{"hw"}},
		{"plughw:Audio", 2, 0, []string{"plug", "hw"}},
		{"plughw:5,0", 5, 0, []string{"plug", "hw"}},
		{"out", 2, 1, []string{"plug", "hw"}},
		{"hwcard", 2, 1, []string{"hw"}},
		{"toHw", 2, 1, []string{"empty", "hw"}},
		{"toPlughw", 5, 0, []string{"empty", "plug", "hw"}},
	}
	for _, tc := range resolved {
		chain, ok := ResolveOutputChain(root, tc.device, cards)
		if !ok || chain.Card != tc.card || chain.Device != tc.pcm || !slices.Equal(chain.Stages, tc.stages) {
			t.Errorf("%s = %+v, %v; want card %d device %d %v", tc.device, chain, ok, tc.card, tc.pcm, tc.stages)
		}
	}
	// A device the card resolver names by card but whose stages live in
	// alsa-lib's own files (front, plug's slave argument), a plugin stage
	// that converts or mixes, a cycle and a name nothing defines have no
	// chain.
	for _, device := range []string{
		"front:Audio", "plug:2", "toFront",
		"throughDmix", "throughSoftvol", "throughRate", "throughRoute", "plugOverDmix",
		"a", "nothing", "", "hw:9,0",
	} {
		if chain, ok := ResolveOutputChain(root, device, cards); ok {
			t.Errorf("%q has a chain: %+v", device, chain)
		}
	}
	// front: and a slave that is front: still name a card for the outputs
	// route; only the chain is unknown.
	for _, device := range []string{"front:Audio", "toFront"} {
		if _, _, ok := ResolveOutputDevice(root, device, cards); !ok {
			t.Errorf("%s no longer resolves to a card", device)
		}
	}
}

// TestAChainIsWhereTheCardResolverGoes holds the chain to the card
// resolver it extends: every device either has no chain or has the card
// and PCM device ResolveOutputDevice gives it.
func TestAChainIsWhereTheCardResolverGoes(t *testing.T) {
	root := filepath.Join("testdata", "volumio-asound-chain")
	cards := volumioCards(t)
	for _, device := range []string{"volumio", "default", "volumioOutput", "volumioHw", "hw:2", "plughw:Audio", "front:Audio", "pulse", "jack", "hw:0,0"} {
		chain, ok := ResolveOutputChain(root, device, cards)
		card, pcm, found := ResolveOutputDevice(root, device, cards)
		if ok && (!found || card != chain.Card || pcm != chain.Device) {
			t.Errorf("%s: chain %+v, card resolver %d, %d, %v", device, chain, card, pcm, found)
		}
		if ok && (len(chain.Stages) == 0 || chain.Stages[len(chain.Stages)-1] != "hw") {
			t.Errorf("%s: a chain that does not end at hw: %v", device, chain.Stages)
		}
	}
}

func TestPCMFormatBits(t *testing.T) {
	cases := []struct {
		format string
		bits   int
		signed bool
		ok     bool
	}{
		{"S16_LE", 16, true, true},
		{"S16_BE", 16, true, true},
		{"S24_3LE", 24, true, true},
		{"S24_LE", 24, true, true},
		{"S32_LE", 32, true, true},
		{"S20_LE", 20, true, true},
		{"S20_3LE", 20, true, true},
		{"S18_3LE", 18, true, true},
		{"U16_LE", 16, false, true},
		{"S8", 8, true, true},
		{"U8", 8, false, true},
		{"DSD_U32_BE", 0, false, false},
		{"DSD_U8", 0, false, false},
		{"FLOAT_LE", 0, false, false},
		{"FLOAT64_LE", 0, false, false},
		{"SPECIAL", 0, false, false},
		{"IEC958_SUBFRAME_LE", 0, false, false},
		{"MU_LAW", 0, false, false},
		{"S24", 0, false, false},
		{"S24_3", 0, false, false},
		{"s16_le", 0, false, false},
		{"", 0, false, false},
	}
	for _, tc := range cases {
		bits, signed, ok := PCMFormatBits(tc.format)
		if bits != tc.bits || signed != tc.signed || ok != tc.ok {
			t.Errorf("%q = %d, %v, %v; want %d, %v, %v", tc.format, bits, signed, ok, tc.bits, tc.signed, tc.ok)
		}
	}
	for _, f := range []string{"DSD_U8", "DSD_U16_LE", "DSD_U16_BE", "DSD_U32_LE", "DSD_U32_BE"} {
		if !IsDSDFormat(f) {
			t.Errorf("%s is not DSD", f)
		}
	}
	for _, f := range []string{"S32_LE", "SPECIAL", "", "dsd_u32_be"} {
		if IsDSDFormat(f) {
			t.Errorf("%q is DSD", f)
		}
	}
}
