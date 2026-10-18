package api

import (
	"net"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestAHeldBackAddressIsLoggedOnceAndTheLogIsBounded (backlog B457): the
// endpoint list is rebuilt on every health refresh, a QR mint and a
// console panel, so an address the certificate does not name is said once
// per process, the two forms of one IPv4 address are one address, and a
// host whose temporary IPv6 addresses change daily stops being remembered
// at heldBackLogMax.
func TestAHeldBackAddressIsLoggedOnceAndTheLogIsBounded(t *testing.T) {
	rec := loggingtest.Record(t)
	var l heldBackLog
	ula, lan := net.ParseIP("fdcc:cccf:784b::2"), net.ParseIP("192.168.1.20")
	l.note(ula)
	l.note(ula)
	l.note(lan.To16())
	l.note(lan.To4())
	lines := rec.Lines(heldBackMessage)
	if len(lines) != 2 || !strings.Contains(lines[0], ula.String()) || !strings.Contains(lines[1], "addr=192.168.1.20") {
		t.Fatalf("lines = %q, want one for %s and one for %s", lines, ula, lan)
	}
	for i := range heldBackLogMax + 10 {
		l.note(net.IPv4(10, 0, byte(i>>8), byte(i)))
	}
	if n := len(rec.Lines(heldBackMessage)); n != heldBackLogMax {
		t.Errorf("%d lines after %d addresses, want the bound, %d", n, heldBackLogMax+12, heldBackLogMax)
	}
}
