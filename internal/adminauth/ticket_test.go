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

// ticketFiles returns every ticket file in s's directory, by name, with its
// bytes: the files ticketDigestFromName recognises, which are the ones a mint
// writes and a prune may remove.
func ticketFiles(t *testing.T, s *Store) map[string][]byte {
	t.Helper()
	dir := filepath.Dir(s.path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if _, ok := s.ticketDigestFromName(e.Name()); !ok {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = b
	}
	return out
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
	if _, err := os.Stat(s.ticketFilePath(hashTicket(raw))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the expired ticket's file survived its presentation (stat err=%v)", err)
	}
	if n := len(ticketFiles(t, s)); n != 0 {
		t.Errorf("%d ticket files left after redeeming an expired one", n)
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

// TestLiveTicketsAreBounded pins the ceiling as a REFUSAL: the mint that
// would pass maxLiveTickets fails, and every ticket already minted still
// redeems. Evicting the oldest to make room would silently kill a link
// someone is holding, which is worse than a mint an operator can retry once
// a link has been used or has expired.
func TestLiveTicketsAreBounded(t *testing.T) {
	s := ticketStore(t)
	var minted []string
	for i := 0; i < maxLiveTickets; i++ {
		raw, err := s.MintLoginTicket("admin")
		if err != nil {
			t.Fatalf("mint %d of %d, under the ceiling: %v", i+1, maxLiveTickets, err)
		}
		minted = append(minted, raw)
	}
	for i := 0; i < 5; i++ {
		if _, err := s.MintLoginTicket("admin"); err == nil {
			t.Fatal("a mint past the ceiling succeeded; the live-ticket set is unbounded")
		}
	}
	if n := len(ticketFiles(t, s)); n != maxLiveTickets {
		t.Errorf("%d ticket files after the refused mints, want %d", n, maxLiveTickets)
	}
	for i, raw := range minted {
		if user, err := s.RedeemLoginTicket(raw); err != nil || user != "admin" {
			t.Errorf("ticket %d of %d after the refused mints = (%q, %v), want (admin, nil): "+
				"a refused mint evicted it", i+1, maxLiveTickets, user, err)
		}
	}
}

// TestAnEntryThatIsNotAFileIsNeverALiveTicket pins that the mint's prune
// counts only REGULAR files toward the ceiling. A directory, or a link to one,
// named exactly like a ticket file fell into the arm that counts an entry it
// could not read as live (which keeps an unreadable ticket inside the
// ceiling), so maxLiveTickets of them refused every mint. Nothing the bridge
// writes there is anything but a regular file. (Gemini on #1062.)
func TestAnEntryThatIsNotAFileIsNeverALiveTicket(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, at string)
	}{
		{"a directory", func(t *testing.T, at string) {
			if err := os.Mkdir(at, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"a link to a directory", func(t *testing.T, at string) {
			if runtime.GOOS == "windows" {
				t.Skip("creating a symlink needs a privilege a Windows runner may not have")
			}
			if err := os.Symlink(t.TempDir(), at); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ticketStore(t)
			for i := 0; i < maxLiveTickets; i++ {
				tc.plant(t, s.ticketFilePath(hashTicket(strings.Repeat("x", i+1))))
			}
			raw, err := s.MintLoginTicket("admin")
			if err != nil {
				t.Fatalf("mint beside %d entries named like tickets that are not files: %v "+
					"(they were counted as live tickets)", maxLiveTickets, err)
			}
			if user, err := s.RedeemLoginTicket(raw); err != nil || user != "admin" {
				t.Fatalf("redeem = (%q, %v), want (admin, nil)", user, err)
			}
		})
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

// The disk must never hold a usable ticket, only its digest: not in the
// ticket's file, not in its name, and not in any other file in the directory.
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
	ticketPath := s.ticketFilePath(hashTicket(raw))
	if _, err := os.Stat(ticketPath); err != nil {
		t.Fatalf("the ticket's file is not where ticketFilePath names it: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), raw) {
			t.Errorf("%s names the ticket itself", e.Name())
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		if strings.Contains(string(body), raw) {
			t.Errorf("%s contains the ticket itself", e.Name())
		}
	}
	info, err := os.Stat(ticketPath)
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
