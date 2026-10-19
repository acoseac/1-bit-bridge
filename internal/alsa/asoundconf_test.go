package alsa

import (
	"os"
	"path/filepath"
	"testing"
)

func volumioCards(t *testing.T) []Card {
	t.Helper()
	snap, err := (Reader{ProcRoot: filepath.Join("testdata", "volumio-asound-chain", "proc")}).Read()
	if err != nil {
		t.Fatal(err)
	}
	cards := make([]Card, len(snap.Cards))
	for i, c := range snap.Cards {
		cards[i] = c.Card
	}
	return cards
}

func TestResolveOutputDeviceVolumioChain(t *testing.T) {
	root := filepath.Join("testdata", "volumio-asound-chain")
	cards := volumioCards(t)
	card, pcm, ok := ResolveOutputDevice(root, "volumio", cards)
	if !ok || card != 2 || pcm != 0 {
		t.Fatalf("volumio = %d, %d, %v; want card 2 pcm 0", card, pcm, ok)
	}
}

func TestResolveOutputDevice(t *testing.T) {
	cards := []Card{{Index: 0, ID: "vc4hdmi0"}, {Index: 2, ID: "Audio"}, {Index: 5, ID: "A20"}}
	root := t.TempDir()
	direct := []struct {
		device string
		card   int
		pcm    int
	}{
		{"hw:2,0", 2, 0},
		{"hw:CARD=Audio,DEV=0", 2, 0},
		{"plughw:Audio", 2, 0},
	}
	for _, tc := range direct {
		card, pcm, ok := ResolveOutputDevice(root, tc.device, cards)
		if !ok || card != tc.card || pcm != tc.pcm {
			t.Errorf("%s = %d, %d, %v", tc.device, card, pcm, ok)
		}
	}

	dir := filepath.Join(root, "etc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
pcm.throughDmix {
    type dmix
    slave.pcm "hwcard"
}
pcm.throughSoftvol {
    type softvol
    slave.pcm "hwcard"
}
pcm.a { type empty
    slave.pcm "b"
}
pcm.b { type empty
    slave.pcm "a"
}
pcm.p0 { type empty
    slave.pcm "p1" }
pcm.p1 { type empty
    slave.pcm "p2" }
pcm.p2 { type empty
    slave.pcm "p3" }
pcm.p3 { type empty
    slave.pcm "p4" }
pcm.p4 { type empty
    slave.pcm "p5" }
pcm.p5 { type empty
    slave.pcm "p6" }
pcm.p6 { type empty
    slave.pcm "p7" }
pcm.p7 { type empty
    slave.pcm "p8" }
pcm.p8 { type empty
    slave.pcm "p9" }
pcm.p9 { type hw
    card 2 }
pcm.q0 { type empty
    slave.pcm "q1" }
pcm.q1 { type empty
    slave.pcm "q2" }
pcm.q2 { type empty
    slave.pcm "q3" }
pcm.q3 { type empty
    slave.pcm "q4" }
pcm.q4 { type empty
    slave.pcm "q5" }
pcm.q5 { type empty
    slave.pcm "q6" }
pcm.q6 { type empty
    slave.pcm "q7" }
pcm.q7 { type empty
    slave.pcm "q8" }
pcm.q8 { type hw
    card 2 }
`
	if err := os.WriteFile(filepath.Join(dir, "asound.conf"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if card, pcm, ok := ResolveOutputDevice(root, "out", cards); !ok || card != 2 || pcm != 1 {
		t.Fatalf("slave block = %d, %d, %v", card, pcm, ok)
	}
	for _, name := range []string{"throughDmix", "throughSoftvol", "a"} {
		if _, _, ok := ResolveOutputDevice(root, name, cards); ok {
			t.Fatalf("%s resolved", name)
		}
	}
	if _, _, ok := ResolveOutputDevice(root, "p0", cards); ok {
		t.Fatal("a chain longer than 8 resolved")
	}
	if card, pcm, ok := ResolveOutputDevice(root, "q0", cards); !ok || card != 2 || pcm != 0 {
		t.Fatalf("eight hops = %d, %d, %v", card, pcm, ok)
	}
	if _, _, ok := ResolveOutputDevice(t.TempDir(), "volumio", cards); ok {
		t.Fatal("a missing asound.conf resolved")
	}
}

func TestDuplicatePCMIsUnresolved(t *testing.T) {
	cards := []Card{{Index: 0, ID: "vc4hdmi0"}, {Index: 2, ID: "Audio"}, {Index: 5, ID: "A20"}}
	root := t.TempDir()
	etc := filepath.Join(root, "etc")
	if err := os.MkdirAll(filepath.Join(etc, "alsa", "conf.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	twice := `
pcm.out { type hw
    card 0 }
pcm.out { type hw
    card "Audio" }
pcm.bang { type hw
    card 5 }
pcm.!bang { type hw
    card "Audio" }
`
	if err := os.WriteFile(filepath.Join(etc, "asound.conf"), []byte(twice), 0o644); err != nil {
		t.Fatal(err)
	}
	if card, pcm, ok := ResolveOutputDevice(root, "out", cards); ok {
		t.Errorf("twice = %d, %d, %v; want unresolved", card, pcm, ok)
	}
	if card, pcm, ok := ResolveOutputDevice(root, "bang", cards); ok {
		t.Errorf("pcm.!name = %d, %d, %v; want unresolved", card, pcm, ok)
	}

	across := t.TempDir()
	etc = filepath.Join(across, "etc")
	if err := os.MkdirAll(filepath.Join(etc, "alsa", "conf.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "asound.conf"), []byte("pcm.out { type hw\n    card \"Audio\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "alsa", "conf.d", "10-first.conf"), []byte("pcm.out { type hw\n    card 0 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if card, pcm, ok := ResolveOutputDevice(across, "out", cards); ok {
		t.Errorf("across files = %d, %d, %v; want unresolved", card, pcm, ok)
	}

	only := t.TempDir()
	if err := os.MkdirAll(filepath.Join(only, "etc", "alsa", "conf.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(only, "etc", "alsa", "conf.d", "10.conf"), []byte("pcm.out { type hw\n    card \"Audio\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if card, pcm, ok := ResolveOutputDevice(only, "out", cards); !ok || card != 2 || pcm != 0 {
		t.Fatalf("conf.d only = %d, %d, %v", card, pcm, ok)
	}
}

func TestDuplicateKeyIsOpaque(t *testing.T) {
	cards := []Card{{Index: 0, ID: "vc4hdmi0"}, {Index: 2, ID: "Audio"}, {Index: 5, ID: "A20"}}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `
pcm.card {
    type hw
    card 5
    card "Audio"
}
pcm.dev {
    type hw
    card "Audio"
    device 0
    device 1
}
pcm.typ {
    type empty
    slave.pcm "hwcard"
    type hw
    card 5
}
pcm.hwcard {
    type hw
    card "Audio"
}
`
	if err := os.WriteFile(filepath.Join(root, "etc", "asound.conf"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if card, pcm, ok := ResolveOutputDevice(root, "card", cards); ok {
		t.Errorf("second card = %d, %d, %v; want unresolved", card, pcm, ok)
	}
	if card, pcm, ok := ResolveOutputDevice(root, "dev", cards); ok {
		t.Errorf("second device = %d, %d, %v; want unresolved", card, pcm, ok)
	}
	if card, pcm, ok := ResolveOutputDevice(root, "typ", cards); ok {
		t.Errorf("second type = %d, %d, %v; want unresolved", card, pcm, ok)
	}
	if card, pcm, ok := ResolveOutputDevice(root, "hwcard", cards); !ok || card != 2 || pcm != 0 {
		t.Fatalf("one card = %d, %d, %v", card, pcm, ok)
	}
}

func TestSlaveDeviceString(t *testing.T) {
	cards := []Card{{Index: 0, ID: "vc4hdmi0"}, {Index: 2, ID: "Audio"}, {Index: 5, ID: "A20"}}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `
pcm.out {
    type plug
    slave.pcm "hw:2,0"
}
pcm.plugout {
    type empty
    slave.pcm "plughw:CARD=Audio,DEV=0"
}
pcm.named {
    type empty
    slave.pcm "hw:0,0"
}
pcm."hw:0,0" {
    type hw
    card "Audio"
}
pcm.other {
    type plug
    slave.pcm "default:CARD=Audio"
}
`
	if err := os.WriteFile(filepath.Join(root, "etc", "asound.conf"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if card, pcm, ok := ResolveOutputDevice(root, "out", cards); !ok || card != 2 || pcm != 0 {
		t.Errorf("hw slave = %d, %d, %v; want card 2 pcm 0", card, pcm, ok)
	}
	if card, pcm, ok := ResolveOutputDevice(root, "plugout", cards); !ok || card != 2 || pcm != 0 {
		t.Errorf("plughw slave = %d, %d, %v; want card 2 pcm 0", card, pcm, ok)
	}
	if card, pcm, ok := ResolveOutputDevice(root, "named", cards); !ok || card != 0 || pcm != 0 {
		t.Errorf("named hw string = %d, %d, %v; want card 0", card, pcm, ok)
	}
	if card, pcm, ok := ResolveOutputDevice(root, "hw:0,0", cards); !ok || card != 0 || pcm != 0 {
		t.Errorf("device hw:0,0 = %d, %d, %v; want card 0", card, pcm, ok)
	}
	if _, _, ok := ResolveOutputDevice(root, "other", cards); ok {
		t.Error("default:CARD resolved")
	}
}

func TestNonPCMStanzaIsSkipped(t *testing.T) {
	cards := volumioCards(t)
	root := filepath.Join("testdata", "volumio-with-ctl")
	if card, pcm, ok := ResolveOutputDevice(root, "volumio", cards); !ok || card != 2 || pcm != 0 {
		t.Errorf("volumio with ctl = %d, %d, %v; want card 2 pcm 0", card, pcm, ok)
	}
	semi := t.TempDir()
	if err := os.MkdirAll(filepath.Join(semi, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "pcm.semi { type hw; card \"Audio\", }\n"
	if err := os.WriteFile(filepath.Join(semi, "etc", "asound.conf"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if card, pcm, ok := ResolveOutputDevice(semi, "semi", cards); !ok || card != 2 || pcm != 0 {
		t.Errorf("separators = %d, %d, %v; want card 2 pcm 0", card, pcm, ok)
	}
	for _, extra := range []string{"@hooks [0]", "<include>", "@args.NAME 1"} {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "etc"), 0o755); err != nil {
			t.Fatal(err)
		}
		text := "pcm.out { type hw\n    card \"Audio\" }\n" + extra + "\n"
		if err := os.WriteFile(filepath.Join(dir, "etc", "asound.conf"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := ResolveOutputDevice(dir, "out", cards); ok {
			t.Errorf("%s resolved", extra)
		}
	}
}

func TestStringEscapeIsOnlyQuoteOrBackslash(t *testing.T) {
	cards := []Card{{Index: 2, ID: "Audio"}, {Index: 5, ID: "A20"}}
	bad := []struct {
		name string
		body string
	}{
		{"letter", "pcm.out { type hw\n    card \"Au\\dio\" }\n"},
		{"newline", "pcm.keep { type hw\n    card \"Audio\" }\npcm.bad { type hw\n    card \"\\nAudio\" }\n"},
		{"tab", "pcm.keep { type hw\n    card \"Audio\" }\npcm.bad { type hw\n    card \"\\tAudio\" }\n"},
		{"hex", "pcm.keep { type hw\n    card \"Audio\" }\npcm.bad { type hw\n    card \"\\x41udio\" }\n"},
		{"octal", "pcm.keep { type hw\n    card \"Audio\" }\npcm.bad { type hw\n    card \"\\060\" }\n"},
	}
	for _, tc := range bad {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "etc", "asound.conf"), []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		name := "out"
		if tc.name != "letter" {
			name = "keep"
		}
		if card, pcm, ok := ResolveOutputDevice(root, name, cards); ok {
			t.Errorf("%s = %d, %d, %v; want unresolved", tc.name, card, pcm, ok)
		}
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	good := `
pcm.quote {
    type empty
    slave.pcm "a\"b"
}
pcm."a\"b" {
    type hw
    card 5
}
pcm.back {
    type empty
    slave.pcm "a\\b"
}
pcm."a\\b" {
    type hw
    card "Audio"
}
pcm.apos {
    type empty
    slave.pcm "a\'b"
}
pcm."a\'b" {
    type hw
    card 5
}
`
	if err := os.WriteFile(filepath.Join(root, "etc", "asound.conf"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if card, pcm, ok := ResolveOutputDevice(root, "quote", cards); !ok || card != 5 || pcm != 0 {
		t.Errorf("quote = %d, %d, %v", card, pcm, ok)
	}
	if card, pcm, ok := ResolveOutputDevice(root, "back", cards); !ok || card != 2 || pcm != 0 {
		t.Errorf("backslash = %d, %d, %v", card, pcm, ok)
	}
	if card, pcm, ok := ResolveOutputDevice(root, "apos", cards); !ok || card != 5 || pcm != 0 {
		t.Errorf("apostrophe = %d, %d, %v", card, pcm, ok)
	}
}

func TestUnparsedFileDropsTheRest(t *testing.T) {
	cards := []Card{{Index: 0, ID: "vc4hdmi0"}, {Index: 2, ID: "Audio"}}
	write := func(t *testing.T, root, rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	write(t, root, filepath.Join("etc", "alsa", "conf.d", "10.conf"), "pcm.out {\n    type hw\n    card 0\n}\n")
	write(t, root, filepath.Join("etc", "asound.conf"), "pcm.!out {\n    @args.CARD { type string }\n    type hw\n    card \"Audio\"\n}\n")
	if card, pcm, ok := ResolveOutputDevice(root, "out", cards); ok {
		t.Errorf("unparsed asound.conf = %d, %d, %v; want unresolved", card, pcm, ok)
	}

	unreadable := t.TempDir()
	write(t, unreadable, filepath.Join("etc", "alsa", "conf.d", "10.conf"), "pcm.out {\n    type hw\n    card 0\n}\n")
	if err := os.Mkdir(filepath.Join(unreadable, "etc", "asound.conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if card, pcm, ok := ResolveOutputDevice(unreadable, "out", cards); ok {
		t.Errorf("unreadable asound.conf = %d, %d, %v; want unresolved", card, pcm, ok)
	}

	only := t.TempDir()
	write(t, only, filepath.Join("etc", "alsa", "conf.d", "10.conf"), "pcm.out {\n    type hw\n    card \"Audio\"\n}\n")
	if card, pcm, ok := ResolveOutputDevice(only, "out", cards); !ok || card != 2 || pcm != 0 {
		t.Fatalf("missing asound.conf = %d, %d, %v", card, pcm, ok)
	}
}

func TestAtDirectiveInsideSkippedBlock(t *testing.T) {
	cards := []Card{{Index: 2, ID: "Audio"}}
	for _, body := range []string{
		"ctl.!default {\n    type hw\n    @hooks [0]\n}\npcm.out { type hw\n    card \"Audio\" }\n",
		"defaults.pcm {\n    @func getenv\n}\npcm.out { type hw\n    card \"Audio\" }\n",
		"ctl.name {\n    @args.CARD { type string }\n}\npcm.out { type hw\n    card \"Audio\" }\n",
	} {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "etc", "asound.conf"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if card, pcm, ok := ResolveOutputDevice(root, "out", cards); ok {
			t.Errorf("at inside block = %d, %d, %v; want unresolved\n%s", card, pcm, ok, body)
		}
	}
}
