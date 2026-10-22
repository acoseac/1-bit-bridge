package alsa

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// TestAPlugThatRoutesHasNoChain: a plug stage passes samples through only
// while it has nothing to convert, and a transfer table or a route policy
// is the route plugin inside it, which can attenuate or swap channels at
// the same format, rate and channels. Such a plug is opaque in every form,
// a dotted ttable entry, a ttable block, route_policy, so it has no chain,
// and it is the route stage the reading warns of. A ttable entry whose
// value is a float makes the file one this parser does not read, which
// names no stage.
func TestAPlugThatRoutesHasNoChain(t *testing.T) {
	cards := []Card{{Index: 2, ID: "Audio"}}
	hw := "pcm.hwcard {\n    type hw\n    card \"Audio\"\n}\n"
	for name, tc := range map[string]struct{ plug, stage string }{
		"a dotted ttable entry": {"pcm.out { type plug\n    slave.pcm \"hwcard\"\n    ttable.0.0 1 }\n", "route"},
		"a ttable block":        {"pcm.out { type plug\n    slave.pcm \"hwcard\"\n    ttable { 0 { 0 0.5 } 1 { 1 0.5 } } }\n", "route"},
		"a route policy":        {"pcm.out { type plug\n    slave.pcm \"hwcard\"\n    route_policy \"average\" }\n", "route"},
		"a float ttable entry":  {"pcm.out { type plug\n    slave.pcm \"hwcard\"\n    ttable.0.0 0.5 }\n", ""},
	} {
		root := t.TempDir()
		writeTree(t, root, map[string]string{"etc/asound.conf": hw + tc.plug})
		if chain, ok := ResolveOutputChain(root, "out", cards); ok {
			t.Errorf("%s: %+v", name, chain)
		}
		if got := LoadPCMConfig(root).SampleStage("out"); got != tc.stage {
			t.Errorf("%s: stage %q, want %q", name, got, tc.stage)
		}
	}
}

// TestAFileOfTheConfigurationThatCannotBeReadIsUnread: a file that is there
// and cannot be read (an open or read error, what is not a regular file, a
// file past the read's bounds) leaves the configuration unread, which the
// reading takes for a configuration it did not have; a file it read that
// holds what the parser does not model, and a file that is not there, do
// not. Either one that is there leaves no definition used.
func TestAFileOfTheConfigurationThatCannotBeReadIsUnread(t *testing.T) {
	plain := "pcm.hwcard { type hw\n    card \"Audio\" }\n"
	tooMany := map[string]string{}
	for i := 0; i <= maxAsoundFiles; i++ {
		tooMany[fmt.Sprintf("etc/alsa/conf.d/%03d.conf", i)] = "# nothing\n"
	}
	for name, tc := range map[string]struct {
		files      map[string]string
		dir        string
		unread, ok bool
	}{
		"nothing there":                  {files: map[string]string{}, ok: true},
		"asound.conf alone":              {files: map[string]string{"etc/asound.conf": plain}, ok: true},
		"asound.conf a directory":        {files: map[string]string{"etc/alsa/conf.d/10-a.conf": plain}, dir: "etc/asound.conf", unread: true},
		"a conf.d entry a directory":     {files: map[string]string{"etc/asound.conf": plain}, dir: "etc/alsa/conf.d/20-b.conf", unread: true},
		"a file past the read's bounds":  {files: map[string]string{"etc/asound.conf": plain + strings.Repeat("#\n", maxFileBytes)}, unread: true},
		"more files than the read takes": {files: tooMany, unread: true},
		"a top-level @hooks":             {files: map[string]string{"etc/asound.conf": plain + "@hooks [ ]\n"}},
		"a value the parser refuses":     {files: map[string]string{"etc/asound.conf": "pcm.soft { type softvol\n    max_dB 0.0 }\n"}},
	} {
		root := t.TempDir()
		writeTree(t, root, tc.files)
		if tc.dir != "" {
			if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(tc.dir)), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		c := LoadPCMConfig(root)
		if c.Unread() != tc.unread || c.ok != tc.ok {
			t.Errorf("%s: unread %v ok %v, want unread %v ok %v", name, c.Unread(), c.ok, tc.unread, tc.ok)
		}
	}
}

// TestTheFirstStageThatChangesSamplesIsNamed: the walk from a device goes
// through empty and plug stages as the files define them and names the
// first stage of a plugin that changes samples, whatever else its block
// holds: a plug with a transfer table or a route policy is the route plugin
// inside it, and alsa-lib's dmix, as a dmix: device string or the name dmix
// the files do not define, is dmix. A chain through empty, plug and hw
// alone, any other device string, a stage it does not follow, a definition
// whose type is not one value, and a name in a configuration not read whole
// name none.
func TestTheFirstStageThatChangesSamplesIsNamed(t *testing.T) {
	body := `
pcm.hwcard { type hw
    card "Audio" }
pcm.soft {
    type softvol
    slave.pcm "hwcard"
    control { name "Soft"; card 2 }
}
pcm.mixed {
    type dmix
    ipc_key 1024
    slave { pcm "hw:2,0"; period_time 0 }
}
pcm.resampled { type rate
    slave.pcm "hwcard"
    converter "samplerate_best" }
pcm.routed { type route
    slave.pcm "hwcard"
    ttable.0.0 1 }
pcm.filtered { type ladspa
    slave.pcm "hwcard" }
pcm.toSoft { type empty
    slave.pcm "plugSoft" }
pcm.plugSoft { type plug
    slave.pcm "soft" }
pcm.passthrough { type plug
    slave.pcm "hwcard" }
pcm.routingPlug { type plug
    slave.pcm "soft"
    ttable.0.0 1 }
pcm.policyPlug { type plug
    slave.pcm "hwcard"
    route_policy "average" }
pcm.tablePlug { type plug
    slave.pcm "hwcard"
    ttable { 0 { 1 1 } 1 { 0 1 } } }
pcm.plugOverDmixString { type plug
    slave.pcm "dmix:2,0" }
pcm.emptyOverDmixString { type empty
    slave { pcm "dmix:CARD=Audio,DEV=0" } }
pcm.plugOverDmix { type plug
    slave.pcm "dmix" }
pcm.twice { type softvol
    type plug
    slave.pcm "hwcard" }
pcm.a { type empty
    slave.pcm "b" }
pcm.b { type empty
    slave.pcm "a" }
pcm.dup { type softvol
    slave.pcm "hwcard" }
pcm.dup { type plug
    slave.pcm "hwcard" }
`
	root := t.TempDir()
	writeTree(t, root, map[string]string{"etc/asound.conf": body})
	c := LoadPCMConfig(root)
	for device, want := range map[string]string{
		"soft": "softvol", "mixed": "dmix", "resampled": "rate", "routed": "route", "filtered": "ladspa",
		"toSoft": "softvol", " toSoft ": "softvol",
		"routingPlug": "route", "policyPlug": "route", "tablePlug": "route",
		"dmix:2,0": "dmix", " dmix:CARD=Audio,DEV=0 ": "dmix", "dmix": "dmix",
		"plugOverDmixString": "dmix", "emptyOverDmixString": "dmix", "plugOverDmix": "dmix",
		"passthrough": "", "hwcard": "", "hw:2,0": "", "plughw:2,0": "", "front:2": "", "dmixer": "",
		"twice": "", "a": "", "dup": "", "nothing": "", "": "",
	} {
		if got := c.SampleStage(device); got != want {
			t.Errorf("%q: %q, want %q", device, got, want)
		}
	}
	for _, typ := range SampleStageTypes() {
		if !slices.Contains([]string{"softvol", "rate", "dmix", "route", "ladspa"}, typ) {
			t.Errorf("a stage type the walk was not shown naming: %s", typ)
		}
	}
	// The definitions in a file read whole, beside one that is not: no
	// definition of the configuration is used.
	hooked := t.TempDir()
	writeTree(t, hooked, map[string]string{
		"etc/alsa/conf.d/10-soft.conf": body,
		"etc/asound.conf":              "@hooks [ { func load files [ \"/etc/more.conf\" ] } ]\n",
	})
	if got := LoadPCMConfig(hooked).SampleStage("soft"); got != "" {
		t.Errorf("a configuration not read whole named %q", got)
	}
	// A device string names its plugin without the files; a name is looked
	// up in them, and one that may redefine dmix is not read.
	if got := LoadPCMConfig(hooked).SampleStage("dmix:2,0"); got != "dmix" {
		t.Errorf("a dmix: device string beside a configuration not read whole: %q", got)
	}
	if got := LoadPCMConfig(hooked).SampleStage("dmix"); got != "" {
		t.Errorf("the name dmix in a configuration not read whole: %q", got)
	}
	// The files' own dmix is what they define.
	redefined := t.TempDir()
	writeTree(t, redefined, map[string]string{
		"etc/asound.conf": "pcm.hwcard { type hw\n    card \"Audio\" }\npcm.dmix { type plug\n    slave.pcm \"hwcard\" }\n",
	})
	if got := LoadPCMConfig(redefined).SampleStage("dmix"); got != "" {
		t.Errorf("a dmix the files define as a plug to hw: %q", got)
	}
	if got := LoadPCMConfig(filepath.Join("testdata", "volumio-asound-chain")).SampleStage("volumio"); got != "" {
		t.Errorf("Volumio's chain: %q", got)
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
