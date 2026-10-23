package alsa

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// These are synthetic configuration files, shared with the verdict tests.
func TestPCMValueFixturesKeepResolutionAndStages(t *testing.T) {
	for _, name := range []string{"float", "negative-real", "negative-integer", "path", "word", "numeric-strings", "byte-strings"} {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata", "asound-values", name+".conf"))
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			writeTree(t, root, map[string]string{"etc/asound.conf": string(body)})
			c := LoadPCMConfig(root)
			cards := []Card{{Index: 2, ID: "Audio"}}
			if !c.ok || c.Unread() {
				t.Fatalf("configuration ok=%v unread=%v", c.ok, c.Unread())
			}
			if card, dev, ok := c.ResolveOutputDevice("keep", cards); !ok || card != 2 || dev != 1 {
				t.Errorf("keep = %d,%d,%v", card, dev, ok)
			}
			if chain, ok := c.ResolveOutputChain("keep", cards); !ok || chain.Card != 2 || chain.Device != 1 || !slices.Equal(chain.Stages, []string{"empty", "hw"}) {
				t.Errorf("keep chain = %+v,%v", chain, ok)
			}
			stage := "softvol"
			if name == "float" {
				stage = "route"
			}
			for _, device := range []string{"effect", "listen"} {
				if got := c.SampleStage(device); got != stage {
					t.Errorf("%s stage = %q, want %q", device, got, stage)
				}
				if chain, ok := c.ResolveOutputChain(device, cards); ok {
					t.Errorf("sample stage gained a chain: %+v", chain)
				}
				if _, _, ok := c.ResolveOutputDevice(device, cards); ok {
					t.Errorf("sample stage %s gained a card", device)
				}
			}
		})
	}
}

func TestPCMValuesStillRefuseUnsafeFiles(t *testing.T) {
	for _, bad := range []string{
		"@hooks [ ]", "<other.conf>", "@func refer", "@args.CARD { type string }",
		"pcm.x { type hw card <x.conf> }", "pcm.x { values [ <x.conf> ] }",
		"pcm.x { type = hw card = <x.conf> }", "pcm.x { values = [ <x.conf> ] }",
		"pcm.x { type hw card = }", "pcm.x =", "pcm.x = = { type hw }",
		"pcm.x { values [ = 2 ] }", `defaults.value "bad\nvalue"`,
		"defaults.value .5", "defaults.value = .5", "x = .5",
		"pcm.x { values [ .5 ] }", "pcm.x { values = [ .5 ] }",
		`defaults.value "unterminated`, "defaults.value /path\x00tail",
		"pcm.keep.card 2", "pcm { other { type hw card 2 } }",
	} {
		t.Run(bad, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, map[string]string{
				"etc/alsa/conf.d/10-good.conf": "pcm.keep { type hw card 2 }\npcm.effect { type softvol }",
				"etc/asound.conf":              bad,
			})
			c := LoadPCMConfig(root)
			if c.ok || c.Unread() {
				t.Errorf("bad syntax ok=%v unread=%v", c.ok, c.Unread())
			}
			if _, _, ok := c.ResolveOutputDevice("keep", []Card{{Index: 2}}); ok {
				t.Error("used another file's definition")
			}
			if got := c.SampleStage("effect"); got != "" {
				t.Errorf("stage from unusable configuration: %q", got)
			}
		})
	}
}

// Unknown or wrongly typed fields can keep a known sample stage, but can
// never promote a definition to a chain, or disguise a second type.
func TestPCMValueKindsDoNotGrantResolution(t *testing.T) {
	for _, field := range []string{"card 2.0", "card -2", "device 1.0", "device -1", "subdevice -1", "slave.pcm 5", "slave.pcm -5", "slave.pcm +5", "slave.pcm /path", "gain 0.5", "gain -5.0"} {
		t.Run(field, func(t *testing.T) {
			prefix := "type empty "
			switch {
			case strings.HasPrefix(field, "card "):
				prefix = "type hw "
			case strings.HasPrefix(field, "device "), strings.HasPrefix(field, "subdevice "):
				prefix = "type hw card 2 "
			case strings.HasPrefix(field, "gain "):
				prefix = `type empty slave.pcm "5" `
			}
			defs, ok := parsePCMFile(`pcm.out { ` + prefix + field + ` } pcm."5" { type hw card 2 } pcm."-5" { type hw card 2 } pcm."+5" { type hw card 2 } pcm."/path" { type hw card 2 }`)
			if !ok {
				t.Fatal("value spoiled the file")
			}
			c := PCMConfig{defs: defs, ok: ok}
			if _, _, ok := c.ResolveOutputDevice("out", []Card{{Index: 2, ID: "2.0"}, {Index: 3, ID: "-2"}}); ok {
				t.Error("unmodeled value gained a card")
			}
		})
	}
	for _, typ := range []string{"0.5", "-5", "softvol"} {
		defs, ok := parsePCMFile(`pcm.effect { type softvol type ` + typ + ` }`)
		if !ok {
			t.Fatalf("second type %s spoiled file", typ)
		}
		if got := (PCMConfig{defs: defs, ok: ok}).SampleStage("effect"); got != "" {
			t.Errorf("second type %s still named %s", typ, got)
		}
	}
}

// Quoting changes the kind; leading '+' and words beginning with letters
// are strings, as are digit/'-' candidates whose numeric conversions fail.
func TestPCMScalarKindsFollowALSA(t *testing.T) {
	for _, tc := range []struct {
		value string
		kind  int
		text  string
	}{
		{"0.5", tokReal, "0.5"}, {"-5.0", tokReal, "-5.0"}, {"-5", tokNumber, "-5"},
		{"2", tokNumber, "2"}, {"012", tokNumber, "10"}, {"0x10", tokNumber, "16"},
		{"08", tokReal, "08"}, {"1e2", tokReal, "1e2"}, {"-0.5e+2", tokReal, "-0.5e+2"},
		{"0x1.8p1", tokReal, "0x1.8p1"}, {"0x1.8", tokReal, "0x1.8"},
		{"/usr/lib/plugin.so", tokString, "/usr/lib/plugin.so"}, {"plugin.so", tokString, "plugin.so"},
		{"word", tokString, "word"}, {"+5", tokString, "+5"}, {"NaN", tokString, "NaN"},
		{`"0.5"`, tokString, "0.5"}, {`'-5'`, tokString, "-5"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			tokens, ok := lexPCM("defaults.value " + tc.value + ";")
			if !ok || len(tokens) != 5 {
				t.Fatalf("tokens = %+v,%v", tokens, ok)
			}
			got := tokens[3]
			if got.kind != tc.kind || got.val != tc.text {
				t.Errorf("value = %+v, want kind %d text %q", got, tc.kind, tc.text)
			}
		})
	}
}

func TestPCMValueDelimiters(t *testing.T) {
	for _, body := range []string{
		"pcm.effect { type softvol; min_dB -5.0,max_dB 0.5# comment\n}",
		"pcm.effect { type softvol min_dB -5.0 }",
		"pcm.effect { type softvol values [ 0.5 -5.0 -5 /usr/lib/a.so word.so ] }",
		"pcm.effect { type softvol limits { min -5.0 max 0.5 } }",
	} {
		defs, ok := parsePCMFile(body)
		if !ok {
			t.Fatalf("refused %s", body)
		}
		if got := (PCMConfig{defs: defs, ok: ok}).SampleStage("effect"); got != "softvol" {
			t.Errorf("stage=%q for %s", got, body)
		}
	}
	for _, body := range []string{
		"pcm.effect { type softvol min_dB 0.5=bad }",
		"pcm.effect { type softvol min_dB 0.5\\bad }",
		"pcm.effect { type softvol min_dB 0.5 } <other.conf>",
		"pcm.effect { type softvol values [ 0.5 } ]",
		"pcm.effect { type softvol min_dB 0.5",
	} {
		if _, ok := parsePCMFile(body); ok {
			t.Errorf("accepted %s", body)
		}
	}
}

// No PCM-file fuzz target preceded B516; seed this one with every new
// token and exercise the parser, both resolvers and the warning's stage.
func FuzzPCMConfigValues(f *testing.F) {
	for _, name := range []string{"float", "negative-real", "negative-integer", "path", "word", "numeric-strings", "byte-strings"} {
		b, err := os.ReadFile(filepath.Join("testdata", "asound-values", name+".conf"))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(string(b))
	}
	for _, value := range []string{"0.5", "-5.0", "-5", "/usr/lib/alsa-lib/plugin.so", "plugin.so", "2chMaster", "0.5x", "-5dB", "1e", "1e9999", ".5", "1_0", "+5", "'0.5'"} {
		f.Add("pcm.effect { type softvol min_dB " + value + " }")
	}
	f.Add("pcm.x = { type = hw card = 2 }")
	f.Add("pcm.x { type hw card <x.conf> }")
	f.Add("pcm.x { values [ <x.conf> ] }")
	f.Add("# Caf\xe9\npcm.effect { type softvol }")
	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > 65536 {
			t.Skip()
		}
		defs, ok := parsePCMFile(src)
		again, okAgain := parsePCMFile(src)
		if ok != okAgain || !reflect.DeepEqual(defs, again) {
			t.Fatal("nondeterministic parse")
		}
		c := PCMConfig{defs: defs, ok: ok}
		cards := []Card{{Index: 2, ID: "Audio"}}
		for _, name := range []string{"keep", "effect", "listen"} {
			card, dev, resolved := c.ResolveOutputDevice(name, cards)
			if chain, known := c.ResolveOutputChain(name, cards); known && (!resolved || chain.Card != card || chain.Device != dev) {
				t.Fatal("chain disagrees with card resolver")
			}
			if stage := c.SampleStage(name); stage != "" && (!ok || !slices.Contains(SampleStageTypes(), stage)) {
				t.Fatalf("unfounded stage %q", stage)
			}
		}
		if !strings.Contains(src, "\x00") {
			for _, guard := range []string{"@hooks [ ]\n", "<other.conf>\n", "pcm.x { type hw card <x.conf> }\n", "pcm.x { values [ <x.conf> ] }\n"} {
				if _, ok := parsePCMFile(guard + src); ok {
					t.Fatal("hooks/include guard bypassed")
				}
			}
		}
	})
}

// parse_value in alsa-lib falls through to STRING when both conversions
// fail. Each word must keep unrelated resolutions and the softvol stage,
// but none of these newly admitted strings can supply routing evidence.
func TestPCMFailedNumbersAreOpaqueStrings(t *testing.T) {
	for _, value := range []string{
		"2chMaster", "2ch", "0.5garbage", "0.0garbage", "-5dB", "--5", "1e", "1_000",
		"0x", "-", "1e9999", "1e-9999", "0b10", "0o10",
	} {
		t.Run(value, func(t *testing.T) {
			tokens, ok := lexPCM("defaults.value " + value)
			if !ok || len(tokens) != 4 {
				t.Fatalf("tokens = %+v,%v", tokens, ok)
			}
			if got := tokens[3]; got.kind != tokString || got.val != value || !got.opaque {
				t.Errorf("value = %+v, want opaque string %q", got, value)
			}
			body := `pcm.keep { type hw card 2 }
			pcm.effect { type softvol slave.pcm "hw:2" control { name ` + value + ` card 2 } }
			pcm.listen { type empty slave.pcm effect }`
			defs, ok := parsePCMFile(body)
			c := PCMConfig{defs: defs, ok: ok}
			cards := []Card{{Index: 2, ID: value}}
			if !ok {
				t.Fatal("string spoiled the file")
			}
			if chain, ok := c.ResolveOutputChain("keep", cards); !ok || chain.Card != 2 || !slices.Equal(chain.Stages, []string{"hw"}) {
				t.Errorf("unrelated chain = %+v,%v", chain, ok)
			}
			if got := c.SampleStage("listen"); got != "softvol" {
				t.Errorf("stage = %q", got)
			}
			for _, field := range []string{"type " + value, "type hw card " + value, "type empty slave.pcm " + value, "type empty slave { pcm " + value + " }"} {
				defs, ok := parsePCMFile(`pcm.out { ` + field + ` } pcm."` + value + `" { type hw card 2 }`)
				c := PCMConfig{defs: defs, ok: ok}
				if !ok {
					t.Fatalf("field spoiled the file: %s", field)
				}
				if _, _, ok := c.ResolveOutputDevice("out", cards); ok {
					t.Errorf("field gained a card: %s", field)
				}
				if chain, ok := c.ResolveOutputChain("out", cards); ok {
					t.Errorf("field gained a chain: %s: %+v", field, chain)
				}
				if got := c.SampleStage("out"); got != "" {
					t.Errorf("field gained a stage: %s: %s", field, got)
				}
			}
		})
	}
}

func TestPCMOptionalEquals(t *testing.T) {
	for _, body := range []string{
		`pcm.x { type = hw card = 2 device = 1 }`,
		`pcm.x = { type hw card 2 device 1 }`,
		"pcm.x # key comment\n = # value comment\n { type=hw; card=2; device=1; }",
		`pcm.direct = { type = hw card = 2 device = 1 } pcm.x = { type = empty slave = { pcm = direct } }`,
		`pcm.direct { type hw card 2 device 1 } pcm.x { type empty slave.pcm = direct }`,
	} {
		t.Run(body, func(t *testing.T) {
			defs, ok := parsePCMFile(body)
			c := PCMConfig{defs: defs, ok: ok}
			if !ok {
				t.Fatalf("refused %s", body)
			}
			if card, dev, ok := c.ResolveOutputDevice("x", []Card{{Index: 2}}); !ok || card != 2 || dev != 1 {
				t.Errorf("resolution = %d,%d,%v: %s", card, dev, ok, body)
			}
			if chain, ok := c.ResolveOutputChain("x", []Card{{Index: 2}}); !ok || chain.Card != 2 || chain.Device != 1 {
				t.Errorf("chain = %+v,%v: %s", chain, ok, body)
			}
		})
	}
	for _, field := range []string{"values = [ 0.5 -5 ]", "control = { name = 2chMaster card = 2 }"} {
		t.Run(field, func(t *testing.T) {
			defs, ok := parsePCMFile("pcm.effect = { type = softvol " + field + " }")
			if !ok || (PCMConfig{defs: defs, ok: ok}).SampleStage("effect") != "softvol" {
				t.Errorf("lost stage for %s", field)
			}
		})
	}
}

// parse_def checks get_nonwhite's result for '.', so blanks and comments
// between components do not end the key (alsa-lib v1.2.14 src/conf.c).
func TestPCMSpacedDottedKeys(t *testing.T) {
	t.Run("slave after blank", func(t *testing.T) {
		defs, ok := parsePCMFile("pcm.direct { type hw card 2 } pcm.x { type plug slave .pcm direct }")
		c := PCMConfig{defs: defs, ok: ok}
		if !ok {
			t.Fatal("spaced slave.pcm spoiled the file")
		}
		cards := []Card{{Index: 2}}
		if card, dev, ok := c.ResolveOutputDevice("x", cards); !ok || card != 2 || dev != 0 {
			t.Errorf("resolution = %d,%d,%v", card, dev, ok)
		}
		if chain, ok := c.ResolveOutputChain("x", cards); !ok || chain.Card != 2 || chain.Device != 0 || !slices.Equal(chain.Stages, []string{"plug", "hw"}) {
			t.Errorf("chain = %+v,%v", chain, ok)
		}
	})
	t.Run("ttable after blank", func(t *testing.T) {
		defs, ok := parsePCMFile("pcm.direct { type hw card 2 } pcm.x { type plug slave.pcm direct ttable.0 .1 0.5 }")
		c := PCMConfig{defs: defs, ok: ok}
		if !ok || c.SampleStage("x") != "route" {
			t.Fatalf("spaced ttable.0.1 lost route: ok=%v stage=%q", ok, c.SampleStage("x"))
		}
		if chain, ok := c.ResolveOutputChain("x", []Card{{Index: 2}}); ok {
			t.Errorf("routing plug gained chain: %+v", chain)
		}
		if _, _, ok := c.ResolveOutputDevice("x", []Card{{Index: 2}}); ok {
			t.Error("routing plug gained card")
		}
	})
	t.Run("key after comment", func(t *testing.T) {
		spaced := "pcm.x # c\n.y { type hw card 2 }"
		got, ok := lexPCM(spaced)
		want, wantOK := lexPCM("pcm.x.y { type hw card 2 }")
		if !ok || !wantOK || !reflect.DeepEqual(got, want) {
			t.Fatalf("tokens = %+v,%v, want %+v,%v", got, ok, want, wantOK)
		}
		// The lexer reads pcm.x.y, but the resolver still refuses PCM
		// definitions with more than one component after pcm.
		if _, ok := parsePCMFile(spaced); ok {
			t.Error("unmodeled nested PCM definition admitted")
		}
	})
}

func TestPCMByteStringsAndComments(t *testing.T) {
	for _, field := range []string{
		"# Caf\xe9\n", "hint.description \"Caf\xe9\"", "hint.description 'Caf\xe9'",
		"hint.description Caf\xe9", "hint.description # Caf\xe9\n \"Cafe\"",
	} {
		t.Run(field, func(t *testing.T) {
			defs, ok := parsePCMFile("pcm.keep { type hw card 2 } pcm.effect { type softvol " + field + " }")
			c := PCMConfig{defs: defs, ok: ok}
			if !ok {
				t.Fatalf("byte string/comment spoiled the file: %q", field)
			}
			if _, _, ok := c.ResolveOutputDevice("keep", []Card{{Index: 2}}); !ok {
				t.Errorf("lost unrelated resolution: %q", field)
			}
			if got := c.SampleStage("effect"); got != "softvol" {
				t.Errorf("lost stage for %q: %q", field, got)
			}
		})
	}
	if _, ok := parsePCMFile("pcm.keep { type hw card 2 } # NUL\x00\n"); ok {
		t.Error("NUL in comment admitted")
	}
	if _, ok := parsePCMFile("pcm.bad\xe9 { type hw card 2 }"); ok {
		t.Error("invalid byte in restricted key admitted")
	}
}
