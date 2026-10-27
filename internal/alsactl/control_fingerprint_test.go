package alsactl

import (
	"encoding/json"
	"testing"
)

func TestActivationFingerprint(t *testing.T) {
	for _, tc := range []struct {
		name          string
		iface, access int
		equal         bool
	}{
		{"volatile_pcm", 3, 5, true}, {"volatile_mixer", 2, 5, false},
		{"volatile_card", 0, 5, false}, {"stable_pcm", 3, 1, false},
		{"stable_mixer", 2, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var a Snapshot
			raw := []byte(`{"Card":5,"Controls":[{"Interface":0}]}`)
			if err := json.Unmarshal(raw, &a); err != nil {
				t.Fatal(err)
			}
			// JSON allows this case to compile on the baseline, which drops Access.
			c, _ := json.Marshal(map[string]any{"Interface": tc.iface, "Access": tc.access, "Name": "gain", "Values": []byte{1}})
			if err := json.Unmarshal(c, &a.Controls[0]); err != nil {
				t.Fatal(err)
			}
			b := a
			b.Controls = append([]Control(nil), a.Controls...)
			b.Controls[0].Values = []byte{2}
			if got := a.Hash() == b.Hash(); got != tc.equal {
				t.Fatalf("activation fingerprint equality=%v want %v", got, tc.equal)
			}
		})
	}
	for _, kind := range []string{"metadata", "membership"} {
		t.Run(kind, func(t *testing.T) {
			a := Snapshot{Card: 5, Controls: []Control{{Interface: 2, Name: "volume", Info: []byte{1}, Values: []byte{1}}}}
			b := a
			b.Controls = append([]Control(nil), a.Controls...)
			if kind == "metadata" {
				b.Controls[0].Info = []byte{2}
			} else {
				b.Controls = append(b.Controls, Control{Name: "new"})
			}
			if a.Hash() == b.Hash() {
				t.Fatal("stable metadata/membership change admitted")
			}
		})
	}
}
