package adminauth

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

// Zero OR LESS means "no opinion", which is what keeps every pre-existing
// caller — and the operator's shell flow — on the budget that is correct for
// them. The negative case is not hypothetical tidiness: it is the arm that
// stops a future `ttl == 0` implementation from passing while a negative
// duration silently mints a ticket that is born expired.
func TestMintLoginTicketTTL_ZeroOrLessMeansTheDefault(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second, -time.Hour} {
		t.Run(ttl.String(), func(t *testing.T) {
			s := ticketStore(t)
			now := time.Now()
			s.now = func() time.Time { return now }
			raw, err := s.MintLoginTicketTTL("admin", ttl)
			if err != nil {
				t.Fatal(err)
			}
			// It lives for the default...
			now = now.Add(LoginTicketTTL - time.Second)
			if _, err := s.RedeemLoginTicket(raw); err != nil {
				t.Fatalf("ttl=%s did not get the default lifetime: %v", ttl, err)
			}

			// ...and no longer than it.
			s2 := ticketStore(t)
			now2 := time.Now()
			s2.now = func() time.Time { return now2 }
			raw2, err := s2.MintLoginTicketTTL("admin", ttl)
			if err != nil {
				t.Fatal(err)
			}
			now2 = now2.Add(LoginTicketTTL + time.Second)
			if _, err := s2.RedeemLoginTicket(raw2); !errors.Is(err, ErrTicketInvalid) {
				t.Fatalf("ttl=%s outlived LoginTicketTTL", ttl)
			}
		})
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

// A ticket store that cannot be WRITTEN must not answer ErrTicketInvalid.
//
// The two are opposite facts. ErrTicketInvalid says something about the
// TICKET — unknown, expired, already spent — and every one of those is the
// holder's problem, recoverable with a fresh link. A failed write says nothing
// about the ticket at all: the record is still on disk and a fresh link will
// fail in exactly the same way. Collapsing the two sends an operator around a
// loop that cannot terminate, and hides the only signal that the store has
// stopped being writable.
func TestRedeemLoginTicketDistinguishesAnUnwritableStore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a directory mode does not gate file creation on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this fixture depends on")
	}
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "adminauth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetInitialPassword("admin", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	// TWO tickets: redeeming one leaves the map non-empty, so the write takes
	// the stage-and-rename path rather than the remove-the-file shortcut an
	// empty map takes.
	raw, err := s.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MintLoginTicket("admin"); err != nil {
		t.Fatal(err)
	}
	// Readable and traversable, not writable: the record is still legible, so
	// the ticket is FOUND and it is only the persist that fails. That is the
	// arm being pinned — a fixture that also broke the read would land on
	// ErrTicketInvalid and prove nothing.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	user, err := s.RedeemLoginTicket(raw)
	if err == nil {
		t.Fatalf("an unwritable store redeemed a ticket as %q", user)
	}
	if errors.Is(err, ErrTicketInvalid) {
		t.Errorf("a store failure was reported as an invalid ticket: %v", err)
	}
}
