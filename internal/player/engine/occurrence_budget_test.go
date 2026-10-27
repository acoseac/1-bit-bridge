package engine

import "testing"

func TestNativeOccurrenceBudgetOwnership(t *testing.T) {
	b := NewOccurrenceBudget(4)
	var held []*OccurrenceReservation
	for range 4 {
		r := b.Reserve()
		if r == nil {
			t.Fatal("early refusal")
		}
		held = append(held, r)
		r.ReleaseEngine()
		r.ReleaseEngine()
	}
	if r := b.Reserve(); r != nil {
		t.Fatal("fifth allocation while discarded openers still own four slots")
	}
	for _, r := range held {
		r.ReleaseBackend()
		r.ReleaseBackend()
	}
	if b.Used() != 0 {
		t.Fatalf("leaked slots: %d", b.Used())
	}
	r := b.Reserve()
	r.ReleaseBackend()
	if b.Used() != 1 {
		t.Fatal("backend freed engine-owned slot")
	}
	r.ReleaseEngine()
	if b.Used() != 0 {
		t.Fatal("last owner did not release slot")
	}
}
