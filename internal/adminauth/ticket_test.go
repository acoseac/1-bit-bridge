package adminauth

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func ticketStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "adminauth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetInitialPassword("admin", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLoginTicketRoundTrip(t *testing.T) {
	s := ticketStore(t)
	raw, err := s.MintLoginTicket("admin")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if len(raw) < 40 {
		t.Errorf("ticket is only %d chars — too little entropy", len(raw))
	}
	user, err := s.RedeemLoginTicket(raw)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if user != "admin" {
		t.Errorf("redeemed as %q, want admin", user)
	}
}

// The whole point of a ticket is that it is spent. A replayable one in a URL
// would be a password with extra steps.
func TestLoginTicketIsSingleUse(t *testing.T) {
	s := ticketStore(t)
	raw, err := s.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemLoginTicket(raw); err != nil {
		t.Fatalf("first redemption failed: %v", err)
	}
	if _, err := s.RedeemLoginTicket(raw); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("second redemption = %v, want ErrTicketInvalid", err)
	}
}

func TestLoginTicketExpires(t *testing.T) {
	s := ticketStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	raw, err := s.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(LoginTicketTTL + time.Second)
	if _, err := s.RedeemLoginTicket(raw); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("an expired ticket redeemed: %v", err)
	}
	// Control: within the window it works.
	now = time.Now()
	s.now = func() time.Time { return now }
	fresh, err := s.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(LoginTicketTTL / 2)
	if _, err := s.RedeemLoginTicket(fresh); err != nil {
		t.Fatalf("control: a ticket inside its window must redeem, got %v", err)
	}
}

// An expired ticket is consumed on presentation too, so it cannot be probed
// repeatedly while the caller waits for a clock edge.
func TestExpiredTicketIsStillConsumed(t *testing.T) {
	s := ticketStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	raw, _ := s.MintLoginTicket("admin")
	now = now.Add(LoginTicketTTL + time.Second)
	_, _ = s.RedeemLoginTicket(raw)
	s.mu.Lock()
	n := len(s.tickets)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("%d tickets still held after redeeming an expired one", n)
	}
}

func TestUnknownTicketsAreRefused(t *testing.T) {
	s := ticketStore(t)
	for _, raw := range []string{"", "not-a-ticket", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		if _, err := s.RedeemLoginTicket(raw); !errors.Is(err, ErrTicketInvalid) {
			t.Errorf("RedeemLoginTicket(%q) = %v, want ErrTicketInvalid", raw, err)
		}
	}
}

// A ticket must never authenticate as an account the store does not have.
func TestCannotMintForAnUnknownUser(t *testing.T) {
	s := ticketStore(t)
	if _, err := s.MintLoginTicket("someone-else"); err == nil {
		t.Fatal("minted a ticket for an account that does not exist")
	}
	if _, err := s.MintLoginTicket(""); err == nil {
		t.Fatal("minted a ticket with no username")
	}
}

func TestLiveTicketsAreBounded(t *testing.T) {
	s := ticketStore(t)
	var lastErr error
	for i := 0; i < maxLiveTickets+5; i++ {
		if _, err := s.MintLoginTicket("admin"); err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		t.Error("minting never refused; the live-ticket set is unbounded")
	}
	s.mu.Lock()
	n := len(s.tickets)
	s.mu.Unlock()
	if n > maxLiveTickets {
		t.Errorf("holding %d tickets, want at most %d", n, maxLiveTickets)
	}
}
