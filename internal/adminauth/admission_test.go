package adminauth

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// The tests in this file follow a login across the window between its check
// and its session (Admission, backlog B881). Verify lets go of the store's
// mutex for bcrypt's compare, and a ticket's redemption returns before its
// session is made, while `bridge admin reset-password` and `bridge admin
// sign-out-everywhere` write the file from a process of their own (b below),
// which takes no lock the running bridge (a, from runningBridge) holds. Each
// test makes the check, lands the other process's write, and only then asks
// for the session, which is where the window is: no seam is needed to park
// in it.

// takeTheSignOut is another console request reaching the running bridge in
// the window: a session check, whose stat sees the file changed and which
// takes what it holds (refreshIfChangedLocked), the sign-out among it, and
// ends the session it checks. Until B881 that was what let a login checked
// before a reset outlive it: the session made after this read was written
// under the new marker, and every later check kept it.
func takeTheSignOut(t *testing.T, a *Store, session string) {
	t.Helper()
	if _, err := a.ValidateSession(session); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("the request in the window = %v, want ErrSessionNotFound: it did not take the sign-out", err)
	}
}

// requireNoSessionAfter requires that a refused login holds nothing: no
// token, no session in the running bridge, and none in the file, which a
// restart would sign in.
func requireNoSessionAfter(t *testing.T, a *Store, path, raw, after string) {
	t.Helper()
	if raw != "" {
		t.Errorf("after %s, the refused login was handed a session token", after)
	}
	if n := a.SessionCount(); n != 0 {
		t.Errorf("after %s, the running bridge holds %d sessions, want none", after, n)
	}
	c, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := c.SessionCount(); n != 0 {
		t.Errorf("after %s, the file holds %d sessions, which a restart signs in; want none", after, n)
	}
}

// TestALoginCheckedBeforeAResetDoesNotOutliveIt is the defect: a login whose
// password was checked against the credential a reset then rotated away, or
// checked before a sign-out, was given a session the reset or the sign-out
// never ended. Another request taking the sign-out in the window is the
// schedule that kept it (the review's case on B881): the session was then made
// after the bridge had taken the marker, and written under it. With no request
// in between, the session's own write took the sign-out and ended the session
// it had just made, while CreateSession still answered success: a cookie that
// signed nothing in.
//
// Every row is refused now, with nothing kept anywhere, and the user signs in
// again, with the password the file holds.
func TestALoginCheckedBeforeAResetDoesNotOutliveIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// elsewhere is the command another process runs in the window.
		elsewhere func(t *testing.T, path string)
		// password is the one the file holds afterwards.
		password string
		// between is whether another request takes the sign-out before the
		// login's session is made.
		between bool
	}{
		{"reset-password, then another request", rotateAndSignOutElsewhere, rotatedPassword, true},
		{"reset-password, the login's write the first to read it", rotateAndSignOutElsewhere, rotatedPassword, false},
		{"sign-out-everywhere, then another request", signOutElsewhere, leakedPassword, true},
		{"sign-out-everywhere, the login's write the first to read it", signOutElsewhere, leakedPassword, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, path, established, _ := runningBridge(t)
			checked := admit(t, a, "admin", leakedPassword)

			tc.elsewhere(t, path)
			if tc.between {
				takeTheSignOut(t, a, established)
			}
			raw, err := a.CreateSession(checked)
			if !errors.Is(err, ErrCredentialsChanged) {
				t.Errorf("the session of a login checked before %s = %v, want ErrCredentialsChanged", tc.name, err)
			}
			requireNoSessionAfter(t, a, path, raw, tc.name)
			if _, err := a.ValidateSession(established); !errors.Is(err, ErrSessionNotFound) {
				t.Errorf("the session signed in before %s = %v, want it ended", tc.name, err)
			}
			// And the user signs in again.
			again := signIn(t, a, "admin", tc.password)
			if _, err := a.ValidateSession(again); err != nil {
				t.Errorf("the login after %s does not sign in: %v", tc.name, err)
			}
		})
	}
}

// TestALoginCheckedBeforeARotationThatKeepsTheSessionsIsRefused proves the
// credential half of the check on its own. A rotation run with
// --keep-sessions writes no sign-out, so the marker does not move and a check
// of the marker alone admits this login; only the password hash it was
// compared with has changed. The sessions already signed in are what the
// operator chose to keep, and stay; the login checked against the password
// the rotation replaced is refused.
func TestALoginCheckedBeforeARotationThatKeepsTheSessionsIsRefused(t *testing.T) {
	a, path, established, _ := runningBridge(t)
	checked := admit(t, a, "admin", leakedPassword)

	rotateElsewhere(t, path) // --keep-sessions
	// Another request in the window: it reads the file, the rotation with it,
	// and keeps its session.
	if _, err := a.ValidateSession(established); err != nil {
		t.Fatalf("the session signed in before a rotation that keeps the sessions = %v, want it kept", err)
	}
	raw, err := a.CreateSession(checked)
	if !errors.Is(err, ErrCredentialsChanged) {
		t.Errorf("the session of a login checked against the password a rotation replaced = %v, want ErrCredentialsChanged", err)
	}
	if raw != "" {
		t.Error("the refused login was handed a session token")
	}
	for name, s := range map[string]*Store{"the running bridge": a, "the file": reopen(t, path)} {
		if n := s.SessionCount(); n != 1 {
			t.Errorf("%s holds %d sessions, want the one kept", name, n)
		}
		if _, err := s.ValidateSession(established); err != nil {
			t.Errorf("%s no longer signs in the session the rotation kept: %v", name, err)
		}
	}
	requireRotationOnDisk(t, path, "a login refused after a rotation that keeps the sessions")
}

// TestALoginTicketRedeemedBeforeASignOutDoesNotOutliveIt is the defect on the
// login link's path: RedeemLoginTicket checks the ticket's account under the
// store's mutex and returns, and the session is made after it, so a sign-out
// landing between the two reached the session as it did a password login's.
func TestALoginTicketRedeemedBeforeASignOutDoesNotOutliveIt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		elsewhere func(t *testing.T, path string)
	}{
		{"reset-password", rotateAndSignOutElsewhere},
		{"sign-out-everywhere", signOutElsewhere},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, path, established, _ := runningBridge(t)
			ticket, err := a.MintLoginTicket("admin")
			if err != nil {
				t.Fatal(err)
			}
			redeemed, err := a.RedeemLoginTicket(ticket)
			if err != nil {
				t.Fatal(err)
			}

			tc.elsewhere(t, path)
			takeTheSignOut(t, a, established)
			raw, err := a.CreateSession(redeemed)
			if !errors.Is(err, ErrCredentialsChanged) {
				t.Errorf("the session of a ticket redeemed before %s = %v, want ErrCredentialsChanged", tc.name, err)
			}
			requireNoSessionAfter(t, a, path, raw, tc.name)
		})
	}
}

// TestALoginTicketRedeemedBeforeARotationThatKeepsTheSessionsSignsIn: a ticket
// is not bound to the password. One minted before a rotation redeems after it
// (TestARotationLeavesLoginTicketsAlone), and one redeemed before a rotation
// that keeps the sessions is not refused for it: its admission holds the
// account it named and the sign-out marker, never the hash.
func TestALoginTicketRedeemedBeforeARotationThatKeepsTheSessionsSignsIn(t *testing.T) {
	a, path, established, _ := runningBridge(t)
	ticket, err := a.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	redeemed, err := a.RedeemLoginTicket(ticket)
	if err != nil {
		t.Fatal(err)
	}

	rotateElsewhere(t, path) // --keep-sessions
	if _, err := a.ValidateSession(established); err != nil {
		t.Fatalf("the session signed in before a rotation that keeps the sessions = %v, want it kept", err)
	}
	raw, err := a.CreateSession(redeemed)
	if err != nil {
		t.Fatalf("the session of a ticket redeemed before a rotation that keeps the sessions = %v, want it signed in", err)
	}
	if _, err := reopen(t, path).ValidateSession(raw); err != nil {
		t.Errorf("the ticket's session is not in the file: %v", err)
	}
}

// TestALoginTicketWhoseAccountIsReplacedBeforeItsSessionIsRefused is
// TestRedemptionRefusesAnAccountReplacedElsewhere for the window after the
// redemption: the account the ticket named is replaced, the store moved aside
// and another made under another name, between the redemption and its session.
func TestALoginTicketWhoseAccountIsReplacedBeforeItsSessionIsRefused(t *testing.T) {
	a, path, _, _ := runningBridge(t)
	ticket, err := a.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	redeemed, err := a.RedeemLoginTicket(ticket)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	b, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetInitialPassword("someone-else", rotatedPassword); err != nil {
		t.Fatal(err)
	}
	if raw, err := a.CreateSession(redeemed); !errors.Is(err, ErrCredentialsChanged) || raw != "" {
		t.Errorf("the session of a ticket whose account was replaced = (%t, %v), want no token and ErrCredentialsChanged", raw != "", err)
	}
	if n := reopen(t, path).SessionCount(); n != 0 {
		t.Errorf("the file holds %d sessions for an account it no longer has, want none", n)
	}
}

// TestALoginTheFileCannotBeReadForIsRefused: the read the session's write is
// built from is where its login is admitted, and a read that fails admits
// nothing. The session is refused and kept nowhere, the file it could not read
// is not written over, and once it reads again nothing of the login lands.
// Until B881 the login was answered as signed in, its session held in memory
// and landed by the next write (the "a login" row
// TestAWriteThatCannotReadTheStoreDoesNotOverwriteIt had).
func TestALoginTheFileCannotBeReadForIsRefused(t *testing.T) {
	a, path, established, clock := runningBridge(t)
	checked := admit(t, a, "admin", leakedPassword)
	saved := damage(t, path)

	raw, err := a.CreateSession(checked)
	if err == nil || raw != "" {
		t.Fatalf("a login whose session's write cannot read the file = (%t, %v), want no token and an error", raw != "", err)
	}
	if got, _ := os.ReadFile(path); string(got) != `{"user": ` {
		t.Fatalf("the refused login wrote over a store it could not read:\n%s", got)
	}
	if n := a.SessionCount(); n != 1 {
		t.Errorf("the running bridge holds %d sessions after the refused login, want its one", n)
	}

	if err := os.WriteFile(path, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	writeTheSessionsHeld(t, a, established, clock)
	c := reopen(t, path)
	if n := c.SessionCount(); n != 1 {
		t.Errorf("the file holds %d sessions once it reads again, want the one signed in before", n)
	}
	if _, err := c.ValidateSession(established); err != nil {
		t.Errorf("the session signed in before is gone from the file: %v", err)
	}
}

// TestALoginWhoseWriteDoesNotLandIsRefused: CreateSession returns every
// failure of its write and keeps no session for it. Here the file changes
// under every attempt (maxCommitAttempts), which is the failure that matters:
// each change after the read the session was admitted on could be a rotation,
// and a session kept in memory after it would be one no read admitted. Until
// B881 the write's error was logged, the login answered as signed in, and the
// session held in memory, pending for the next write.
func TestALoginWhoseWriteDoesNotLandIsRefused(t *testing.T) {
	a, path, established, clock := runningBridge(t)
	checked := admit(t, a, "admin", leakedPassword)
	attempts := 0
	beforeCommitHook = func() {
		attempts++
		touchInPlace(t, path)
	}
	t.Cleanup(func() { beforeCommitHook = nil })

	raw, err := a.CreateSession(checked)
	beforeCommitHook = nil
	if attempts != maxCommitAttempts {
		t.Fatalf("the login's write made %d attempts, want %d: the fixture did not fail it", attempts, maxCommitAttempts)
	}
	if !errors.Is(err, errStoreMoved) || raw != "" {
		t.Fatalf("a login whose write never landed = (%t, %v), want no token and errStoreMoved", raw != "", err)
	}
	if n := a.SessionCount(); n != 1 {
		t.Errorf("the running bridge holds %d sessions after the refused login, want its one", n)
	}
	writeTheSessionsHeld(t, a, established, clock)
	if n := reopen(t, path).SessionCount(); n != 1 {
		t.Errorf("the file holds %d sessions after the next write, want the one signed in before", n)
	}
}

// writeTheSessionsHeld makes the running bridge write every session it holds:
// the activity of session past the debounce, whose write puts down the set.
func writeTheSessionsHeld(t *testing.T, a *Store, session string, clock *testClock) {
	t.Helper()
	clock.t = clock.t.Add(sessionFlushInterval + time.Second)
	if _, err := a.ValidateSession(session); err != nil {
		t.Fatalf("the session signed in before = %v, want it kept", err)
	}
}

// TestTheZeroAdmissionAdmitsNothing: a session needs an Admission only a check
// makes. One constructed outside this package is the zero value, and
// CreateSession refuses it, so no caller can sign a session in without a
// password or a ticket checked.
func TestTheZeroAdmissionAdmitsNothing(t *testing.T) {
	a, path, _, _ := runningBridge(t)
	if raw, err := a.CreateSession(Admission{}); !errors.Is(err, ErrCredentialsChanged) || raw != "" {
		t.Errorf("CreateSession(Admission{}) = (%t, %v), want no token and ErrCredentialsChanged", raw != "", err)
	}
	if n := reopen(t, path).SessionCount(); n != 1 {
		t.Errorf("the file holds %d sessions, want the one signed in before", n)
	}
	if got := (Admission{}).Username(); got != "" {
		t.Errorf("the zero Admission names %q, want no account", got)
	}
}

// TestLoginsRacingNothingButEachOtherAreAdmitted is the guard, green before
// B881 and after: the check holds a login to the credential and the sign-out
// marker, never to the file's bytes, which every session write changes. A
// login whose check another login's session write followed is admitted, and
// so are two checked together whose sessions are made at once.
func TestLoginsRacingNothingButEachOtherAreAdmitted(t *testing.T) {
	a, path, established, _ := runningBridge(t)
	first := admit(t, a, "admin", leakedPassword)
	second := admit(t, a, "admin", leakedPassword)
	third := admit(t, a, "admin", leakedPassword)

	raw := make([]string, 3)
	errs := make([]error, 3)
	raw[0], errs[0] = a.CreateSession(first)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, checked := range []Admission{second, third} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			raw[i+1], errs[i+1] = a.CreateSession(checked)
		}()
	}
	close(start)
	wg.Wait()

	c := reopen(t, path)
	for i := range raw {
		if errs[i] != nil {
			t.Errorf("login %d racing only other logins = %v, want it signed in", i, errs[i])
			continue
		}
		for name, s := range map[string]*Store{"the running bridge": a, "the file": c} {
			if _, err := s.ValidateSession(raw[i]); err != nil {
				t.Errorf("%s does not sign in login %d: %v", name, i, err)
			}
		}
	}
	if _, err := a.ValidateSession(established); err != nil {
		t.Errorf("the session signed in before the logins was ended: %v", err)
	}
}

// reopen opens the file as the next start of the bridge would.
func reopen(t *testing.T, path string) *Store {
	t.Helper()
	c, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
