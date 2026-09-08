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
	n := len(s.readTicketsLocked())
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
	n := len(s.readTicketsLocked())
	s.mu.Unlock()
	if n > maxLiveTickets {
		t.Errorf("holding %d tickets, want at most %d", n, maxLiveTickets)
	}
}

// The regression that unit tests could not see: `bridge admin login-link` runs
// as a separate process from the serving bridge, so a ticket has to survive the
// process that minted it. Two Store values over one path stand in for that.
func TestTicketCrossesProcesses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adminauth.json")

	minting, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := minting.SetInitialPassword("admin", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	raw, err := minting.MintLoginTicket("admin")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// A different Store over the same path — the serving bridge, which never
	// shared memory with the CLI that minted.
	serving, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	user, err := serving.RedeemLoginTicket(raw)
	if err != nil {
		t.Fatalf("a ticket minted by another process did not redeem: %v", err)
	}
	if user != "admin" {
		t.Errorf("redeemed as %q, want admin", user)
	}

	// And it is spent everywhere, not just in the process that redeemed it.
	if _, err := minting.RedeemLoginTicket(raw); !errors.Is(err, ErrTicketInvalid) {
		t.Errorf("the minting process could still redeem a spent ticket: %v", err)
	}
}

// The file must never contain a usable ticket, only its digest.
func TestTicketFileHoldsNoUsableCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adminauth.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetInitialPassword("admin", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	raw, err := s.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(s.ticketPath())
	if err != nil {
		t.Fatalf("reading the ticket file: %v", err)
	}
	if strings.Contains(string(body), raw) {
		t.Fatal("the ticket file contains the ticket itself")
	}
	info, err := os.Stat(s.ticketPath())
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no POSIX permission bits — Go maps the whole mode onto one
	// read-only flag, so a file written 0600 stats as 0666 and the assertion
	// cannot hold there. Guard the ASSERTION, not the stat: an `err == nil &&`
	// form would let the check vanish on any future breakage of the path
	// itself, which is the shape the backup package already records.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("ticket file mode = %04o, want 0600", perm)
		}
	}
}
