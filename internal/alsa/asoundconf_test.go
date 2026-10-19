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
