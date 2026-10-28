package alsactl

import "testing"

func TestSemanticConstraints(t *testing.T) {
	c := Control{Interface: 2, Device: 0, Name: "Volume", Index: 0, Type: 2, Count: 2, Access: 1, Values: []byte{1, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0}}
	want := []Constraint{{Interface: 2, Name: "Volume", Channel: 1, Value: []byte{2, 0, 0, 0, 0, 0, 0, 0}}}
	for _, card := range []int{0, 7} {
		if err := (Snapshot{Card: card, Controls: []Control{c, {Name: "unrelated", ReadError: "unreadable unrelated control", Values: []byte{byte(card)}}}}).CheckConstraints(want); err != nil {
			t.Fatal("semantic match refused:", err)
		}
	}
	c.Values[8] = 3
	if err := (Snapshot{Controls: []Control{c}}).CheckConstraints(want); err == nil {
		t.Fatal("changed constrained channel accepted")
	}
	if err := (Snapshot{}).CheckConstraints(want); err == nil {
		t.Fatal("disappeared constraint accepted")
	}
	c.Values = nil
	if err := (Snapshot{Controls: []Control{c}}).CheckConstraints(want); err == nil {
		t.Fatal("unreadable constraint accepted")
	}
}
