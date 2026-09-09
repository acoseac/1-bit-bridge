package adminauth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A cross-device hand-off needs longer than the shell-local default, and the
// defect was that it could not ask for one: the hosted control plane mints
// while the user is holding a PHONE, the URL then travels to another machine
// and waits for a human to notice it, and at 60 seconds that failed every time.
func TestMintLoginTicketTTL_AnExplicitTTLOutlivesTheDefault(t *testing.T) {
	s := ticketStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }

	raw, err := s.MintLoginTicketTTL("admin", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(LoginTicketTTL + 30*time.Second)
	if _, err := s.RedeemLoginTicket(raw); err != nil {
		t.Fatalf("a 5-minute ticket died at the 60-second default: %v", err)
	}

	// And it is still bounded — the point is a longer life, not an open one.
	now = time.Now()
	s.now = func() time.Time { return now }
	raw2, err := s.MintLoginTicketTTL("admin", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(5*time.Minute + time.Second)
	if _, err := s.RedeemLoginTicket(raw2); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("a ticket past its own explicit ttl redeemed: %v", err)
	}
}

// Zero means "no opinion", which is what keeps every pre-existing caller — and
// the operator's shell flow — on the budget that is correct for them.
func TestMintLoginTicketTTL_ZeroMeansTheDefault(t *testing.T) {
	s := ticketStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	raw, err := s.MintLoginTicketTTL("admin", 0)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(LoginTicketTTL + time.Second)
	if _, err := s.RedeemLoginTicket(raw); !errors.Is(err, ErrTicketInvalid) {
		t.Fatal("ttl=0 did not fall back to LoginTicketTTL")
	}
}

// Refused, not clamped. Quietly shortening it would leave an operator believing
// a link lives longer than it does — this parameter's own bug, inverted.
func TestMintLoginTicketTTL_RefusesMoreThanTheMaximum(t *testing.T) {
	s := ticketStore(t)
	if _, err := s.MintLoginTicketTTL("admin", MaxLoginTicketTTL+time.Second); err == nil {
		t.Fatal("a ttl past the maximum was accepted")
	} else if !strings.Contains(err.Error(), "maximum") {
		t.Errorf("the refusal should name the ceiling: %v", err)
	}
	s.mu.Lock()
	n := len(s.readTicketsLocked())
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("%d tickets held after a REFUSED mint — nothing should have been written", n)
	}
	// Control: the maximum itself is allowed, so the bound is not off by one.
	if _, err := s.MintLoginTicketTTL("admin", MaxLoginTicketTTL); err != nil {
		t.Fatalf("the maximum itself must be allowed: %v", err)
	}
}
