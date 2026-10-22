package alsa

import (
	"os"
	"path/filepath"
	"runtime"
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

// TestAPlugThatRoutesHasNoChain: a plug stage passes samples through only
// while it has nothing to convert, and a transfer table or a route policy
// is the route plugin inside it, which can attenuate or swap channels at
// the same format, rate and channels. Such a plug is opaque in every form:
// a dotted ttable entry, a ttable block, route_policy; a ttable entry
// whose value is a float makes the file one this parser does not read.
func TestAPlugThatRoutesHasNoChain(t *testing.T) {
	cards := []Card{{Index: 2, ID: "Audio"}}
	hw := "pcm.hwcard {\n    type hw\n    card \"Audio\"\n}\n"
	for name, plug := range map[string]string{
		"a dotted ttable entry": "pcm.out { type plug\n    slave.pcm \"hwcard\"\n    ttable.0.0 1 }\n",
		"a ttable block":        "pcm.out { type plug\n    slave.pcm \"hwcard\"\n    ttable { 0 { 0 0.5 } 1 { 1 0.5 } } }\n",
		"a route policy":        "pcm.out { type plug\n    slave.pcm \"hwcard\"\n    route_policy \"average\" }\n",
		"a float ttable entry":  "pcm.out { type plug\n    slave.pcm \"hwcard\"\n    ttable.0.0 0.5 }\n",
	} {
		root := t.TempDir()
		writeTree(t, root, map[string]string{"etc/asound.conf": hw + plug})
		if chain, ok := ResolveOutputChain(root, "out", cards); ok {
			t.Errorf("%s: %+v", name, chain)
		}
	}
}

// TestADeviceStringHasAChainOnlyWhileNothingRedefinesIt: hw:N and
// plughw:N name alsa-lib's own hw and plughw PCMs, which a definition in
// /etc (pcm.!hw, pcm.plughw) replaces, and type hw, plug and empty name
// plugins a pcm_type stanza can hand to another library. So a stage is
// known only from a configuration the bridge read whole that redefines
// neither its PCM nor its type; a definition of another name, or a type
// the chain does not use, leaves it known. The card resolver is unchanged:
// GET /v1/outputs still lists the card such a device names.
func TestADeviceStringHasAChainOnlyWhileNothingRedefinesIt(t *testing.T) {
	cards := []Card{{Index: 2, ID: "Audio"}}
	base := "pcm.hwcard {\n    type hw\n    card \"Audio\"\n}\n" +
		"pcm.toHw { type empty\n    slave.pcm \"hw:Audio,0\" }\n" +
		"pcm.toPlug { type plug\n    slave.pcm \"hwcard\" }\n"
	devices := []string{"hw:2,0", "plughw:2,0", "hwcard", "toHw", "toPlug"}
	for _, tc := range []struct {
		name  string
		extra string
		known []string
	}{
		{"nothing redefined", "", devices},
		{"pcm.!hw", "pcm.!hw { type plug\n    slave.pcm \"hwcard\" }\n", []string{"plughw:2,0", "hwcard", "toPlug"}},
		{"pcm.hw with arguments", "pcm.hw { @args [ CARD ]\n    type hw\n    card $CARD }\n", []string{"plughw:2,0", "hwcard", "toPlug"}},
		{"pcm.plughw", "pcm.plughw { type hw\n    card \"Audio\" }\n", []string{"hw:2,0", "hwcard", "toHw", "toPlug"}},
		{"another name", "pcm.mine { type hw\n    card \"Audio\" }\n", devices},
		{"pcm_type.hw", "pcm_type.hw { lib \"/usr/lib/libother.so\" }\n", nil},
		{"pcm_type.plug", "pcm_type.plug { lib \"/usr/lib/libother.so\" }\n", []string{"hw:2,0", "hwcard", "toHw"}},
		{"pcm_type.!empty", "pcm_type.!empty { lib \"/usr/lib/libother.so\" }\n", []string{"hw:2,0", "plughw:2,0", "hwcard", "toPlug"}},
		{"pcm_type of a type no chain uses", "pcm_type.jack { lib \"/usr/lib/libjack.so\" }\n", devices},
		{"a pcm_type block", "pcm_type {\n    hw { lib \"/usr/lib/libother.so\" }\n}\n", nil},
		{"a file this parser does not read", "@hooks [ { func load files [ \"/etc/more.conf\" ] } ]\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, map[string]string{"etc/asound.conf": base + tc.extra})
			for _, device := range devices {
				_, ok := ResolveOutputChain(root, device, cards)
				if want := slices.Contains(tc.known, device); ok != want {
					t.Errorf("%s: chain %v, want %v", device, ok, want)
				}
			}
			for _, device := range []string{"hw:2,0", "plughw:2,0"} {
				if card, _, ok := ResolveOutputDevice(root, device, cards); !ok || card != 2 {
					t.Errorf("the card resolver no longer resolves %s: %d %v", device, card, ok)
				}
			}
		})
	}
}

// TestAUsersOwnALSAConfigurationIsLookedFor: alsa-lib loads ~/.asoundrc and
// ~/.config/alsa/asoundrc after /etc, so a definition there replaces the
// one the bridge read; the home is the uid's in etc/passwd. Either file is
// there, or what the bridge cannot tell (no passwd, no entry for the uid, a
// home it cannot look in) is unknown.
func TestAUsersOwnALSAConfigurationIsLookedFor(t *testing.T) {
	passwd := "root:x:0:0:root:/root:/bin/bash\nmpd:x:107:29::/var/lib/mpd:/usr/sbin/nologin\nnohome:x:108:29::relative:/bin/false\n"
	for _, tc := range []struct {
		name           string
		files          map[string]string
		uid            int
		present, known bool
	}{
		{"neither file", map[string]string{"etc/passwd": passwd, "var/lib/mpd/music/a": ""}, 107, false, true},
		{"no home at all", map[string]string{"etc/passwd": passwd}, 107, false, true},
		{"~/.asoundrc", map[string]string{"etc/passwd": passwd, "var/lib/mpd/.asoundrc": "pcm.!default { type softvol }\n"}, 107, true, true},
		{"~/.config/alsa/asoundrc", map[string]string{"etc/passwd": passwd, "var/lib/mpd/.config/alsa/asoundrc": ""}, 107, true, true},
		{"a uid passwd does not name", map[string]string{"etc/passwd": passwd}, 4242, false, false},
		{"a home that is not absolute", map[string]string{"etc/passwd": passwd}, 108, false, false},
		{"no passwd", map[string]string{}, 107, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, tc.files)
			present, known := UserConfig(root, tc.uid)
			if present != tc.present || known != tc.known {
				t.Fatalf("present %v known %v, want %v %v", present, known, tc.present, tc.known)
			}
		})
	}
	t.Run("a home the bridge cannot look in", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a POSIX permission bit a non-root reader honours")
		}
		root := t.TempDir()
		writeTree(t, root, map[string]string{"etc/passwd": passwd, "var/lib/mpd/music/a": ""})
		home := filepath.Join(root, "var", "lib", "mpd")
		if err := os.Chmod(home, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(home, 0o755) })
		if present, known := UserConfig(root, 107); known {
			t.Fatalf("a home it cannot look in: present %v known %v", present, known)
		}
	})
}

// TestTheFirstStageThatChangesSamplesIsNamed: the walk from a device goes
// through empty and plug stages as the files define them and names the
// first stage of a plugin that changes samples, whatever else its block
// holds; a chain through empty, plug and hw alone, a device string, a stage
// it does not follow, a definition whose type is not one value, and a
// configuration not read whole name none.
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
		"passthrough": "", "hwcard": "", "hw:2,0": "", "plughw:2,0": "", "routingPlug": "",
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
	hooked := t.TempDir()
	writeTree(t, hooked, map[string]string{"etc/asound.conf": body + "@hooks [ { func load files [ \"/etc/more.conf\" ] } ]\n"})
	if got := LoadPCMConfig(hooked).SampleStage("soft"); got != "" {
		t.Errorf("a configuration not read whole named %q", got)
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
