package engine

import (
	"os"
	"strings"
	"testing"
)

func TestUnreadableReservationRefusesStart(t *testing.T) {
	h := newHarness(t)
	h.be = newFakeBackend(t)
	t.Cleanup(h.be.close)
	h.must("corrupt floor", os.WriteFile(h.reservePath(), []byte("not json"), 0600))
	e, err := New(h.config())
	if err == nil {
		h.eng = e
		t.Cleanup(func() { h.joined(e) })
		h.run()
		t.Fatal("engine accepted an unreadable floor")
	}
	if !strings.Contains(err.Error(), "--discard-floor") {
		t.Fatalf("missing remedy: %v", err)
	}
	b, err := os.ReadFile(h.reservePath())
	if err != nil || string(b) != "not json" {
		t.Fatalf("floor changed: %q %v", b, err)
	}
}
