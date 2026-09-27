package adminauth

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The tests in this file open TWO stores on one adminauth.json, which is how
// the credential is shared in production. `bridge serve` holds one for its
// whole life (the running bridge, `a` below), and `bridge admin
// reset-password`, `bridge admin login-link` and `bridge init` each open
// another in a process of their own. Store.mu serialises neither against the
// other, so a store that decides from what it read at open, or writes what it
// holds in memory, acts on a file another process has since replaced.

const (
	leakedPassword  = "the password that leaked"
	rotatedPassword = "the password it was rotated to"
)

// testClock is a clock a test moves by hand, for the session debounce.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

// runningBridge opens the store a running bridge holds, with the credential
// set to leakedPassword and one console session signed in, and returns it
// with the file's path, the session and the store's clock.
func runningBridge(t *testing.T) (a *Store, path, session string, clock *testClock) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "adminauth.json")
	a, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	clock = &testClock{t: time.Now()}
	a.now = clock.now
	if err := a.SetInitialPassword("admin", leakedPassword); err != nil {
		t.Fatal(err)
	}
	session, err = a.CreateSession("admin")
	if err != nil {
		t.Fatal(err)
	}
	return a, path, session, clock
}

// rotateElsewhere is `bridge admin reset-password` run against the file while
// the bridge runs: a store of its own, opened now, rotating the credential.
func rotateElsewhere(t *testing.T, path string) {
	t.Helper()
	b, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ResetPassword("admin", rotatedPassword); err != nil {
		t.Fatal(err)
	}
}

// requireRotationOnDisk opens the file as the NEXT start of the bridge would,
// and requires that it accepts the rotated password and refuses the leaked
// one.
func requireRotationOnDisk(t *testing.T, path, after string) {
	t.Helper()
	c, err := OpenStore(path)
	if err != nil {
		t.Fatalf("after %s, the store no longer opens: %v", after, err)
	}
	if err := c.Verify("admin", rotatedPassword); err != nil {
		t.Errorf("after %s, the file refuses the rotated password (%v): the running "+
			"bridge wrote the credential it held in memory back over the rotation", after, err)
	}
	if err := c.Verify("admin", leakedPassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("after %s, the file still accepts the leaked password (err=%v)", after, err)
	}
}

// damage replaces the store file with bytes no reader can parse, standing in
// for a file this process cannot read (a foreign writer, a hand edit, a
// credential another user now owns), and returns what was there.
func damage(t *testing.T, path string) (saved []byte) {
	t.Helper()
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"user": `), 0o600); err != nil {
		t.Fatal(err)
	}
	return saved
}

// TestAWriteByTheRunningBridgeKeepsARotationMadeElsewhere is the reported
// defect: `bridge admin reset-password` run against a running public bridge
// was undone by the bridge's next write of the file, which wrote the password
// hash it had loaded at start back over the rotated one. Every session write
// did it, and the shutdown flush of the restart the command advised was one of
// them.
//
// Each row's write is the running store's FIRST act after the rotation, so a
// row cannot pass on a credential another path already re-read.
func TestAWriteByTheRunningBridgeKeepsARotationMadeElsewhere(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, a *Store, session string, clock *testClock)
	}{
		{"a login", func(t *testing.T, a *Store, _ string, _ *testClock) {
			if _, err := a.CreateSession("admin"); err != nil {
				t.Fatal(err)
			}
		}},
		{"session activity past the debounce", func(t *testing.T, a *Store, session string, clock *testClock) {
			clock.t = clock.t.Add(sessionFlushInterval + time.Second)
			if _, err := a.ValidateSession(session); err != nil {
				t.Fatal(err)
			}
		}},
		{"a logout", func(t *testing.T, a *Store, session string, _ *testClock) {
			a.DeleteSession(session)
		}},
		{"the shutdown flush", func(t *testing.T, a *Store, session string, clock *testClock) {
			// Activity inside the debounce window writes nothing and leaves
			// the flush something to land, which is a restart after a console
			// request in the last 30 seconds.
			clock.t = clock.t.Add(time.Second)
			if _, err := a.ValidateSession(session); err != nil {
				t.Fatal(err)
			}
			if err := a.FlushSessions(); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, path, session, clock := runningBridge(t)
			rotateElsewhere(t, path)
			tc.write(t, a, session, clock)
			requireRotationOnDisk(t, path, tc.name)
		})
	}
}

// TestTheRunningBridgeVerifiesARotationMadeElsewhere: the running bridge must
// take the rotated password at its next login attempt, with no write of its
// own in between and no restart. It held the credential it loaded at start, so
// the leaked password kept signing in until the process ended.
func TestTheRunningBridgeVerifiesARotationMadeElsewhere(t *testing.T) {
	a, path, _, _ := runningBridge(t)
	rotateElsewhere(t, path)

	if err := a.Verify("admin", leakedPassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("the running bridge still accepts the leaked password after a rotation (err=%v)", err)
	}
	if err := a.Verify("admin", rotatedPassword); err != nil {
		t.Errorf("the running bridge refuses the rotated password: %v", err)
	}
}

// TestARotationCarriesTheRunningBridgesSessions: the rotating process writes
// the file's session set as it finds it at the write, never the copy it read
// at open. `bridge admin reset-password` waits at a password prompt between
// the two, and the bridge goes on signing sessions in and out meanwhile: a
// session signed in during the prompt was dropped from the file, and one
// signed OUT during it was written back, a logout undone by the next restart.
//
// Rotating the credential leaves the sessions as they are. Whether it should
// end them is a separate decision, and the sessions are the running bridge's.
func TestARotationCarriesTheRunningBridgesSessions(t *testing.T) {
	a, path, signedOut, _ := runningBridge(t)
	b, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// While b waits at its prompt.
	signedIn, err := a.CreateSession("admin")
	if err != nil {
		t.Fatal(err)
	}
	a.DeleteSession(signedOut)
	if err := b.ResetPassword("admin", rotatedPassword); err != nil {
		t.Fatal(err)
	}

	c, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ValidateSession(signedIn); err != nil {
		t.Errorf("a session signed in while the rotation waited is gone from the file: %v", err)
	}
	if _, err := c.ValidateSession(signedOut); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("a session signed out while the rotation waited is back in the file (err=%v)", err)
	}
	if _, err := a.ValidateSession(signedIn); err != nil {
		t.Errorf("the running bridge lost a session to the rotation: %v", err)
	}
}

// TestAWriteThatCannotReadTheStoreDoesNotOverwriteIt: a session write that
// cannot re-read the file writes nothing, because the file may hold a
// credential newer than the one in memory, and the change it held back lands
// once the file reads again, at the latest at the shutdown flush. A
// logout is the case where both halves matter: overwriting undoes a rotation,
// and dropping it undoes the logout at the next restart.
func TestAWriteThatCannotReadTheStoreDoesNotOverwriteIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// write makes the change while the file cannot be read, and returns
		// the session whose state it changed.
		write func(t *testing.T, a *Store, session string) string
		// landed reports whether a store opened afterwards shows the change.
		landed func(c *Store, session string) bool
	}{
		{"a logout",
			func(t *testing.T, a *Store, session string) string {
				a.DeleteSession(session)
				return session
			},
			func(c *Store, session string) bool {
				_, err := c.ValidateSession(session)
				return errors.Is(err, ErrSessionNotFound)
			}},
		{"a login",
			func(t *testing.T, a *Store, _ string) string {
				raw, err := a.CreateSession("admin")
				if err != nil {
					t.Fatalf("a login failed because the store could not be written: %v", err)
				}
				if _, err := a.ValidateSession(raw); err != nil {
					t.Fatalf("the new session does not work in the process that made it: %v", err)
				}
				return raw
			},
			func(c *Store, session string) bool {
				_, err := c.ValidateSession(session)
				return err == nil
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, path, session, _ := runningBridge(t)
			saved := damage(t, path)

			changed := tc.write(t, a, session)
			if got, _ := os.ReadFile(path); string(got) != `{"user": ` {
				t.Fatalf("%s overwrote a store file it could not read:\n%s", tc.name, got)
			}

			if err := os.WriteFile(path, saved, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := a.FlushSessions(); err != nil {
				t.Fatalf("FlushSessions once the file reads again: %v", err)
			}
			c, err := OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.landed(c, changed) {
				t.Errorf("%s made while the file could not be read never reached it: "+
					"the shutdown flush found nothing pending", tc.name)
			}
		})
	}
}

// TestAFailedSessionWriteWaitsOutTheDebounce: a session write that fails
// starts the next debounce window as a successful one does. Otherwise every
// authenticated request after the failure is "due", and a file that stays
// unreadable costs a read and an error line per console request, which is
// the log flood the debounce exists to bound.
func TestAFailedSessionWriteWaitsOutTheDebounce(t *testing.T) {
	a, path, session, clock := runningBridge(t)
	saved := damage(t, path)

	clock.t = clock.t.Add(sessionFlushInterval + time.Second)
	if _, err := a.ValidateSession(session); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != `{"user": ` {
		t.Fatalf("a due write overwrote a store file it could not read:\n%s", got)
	}

	if err := os.WriteFile(path, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	clock.t = clock.t.Add(time.Second)
	if _, err := a.ValidateSession(session); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, saved) {
		t.Error("the request after a failed write wrote again, inside the debounce window")
	}

	clock.t = clock.t.Add(sessionFlushInterval)
	if _, err := a.ValidateSession(session); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); bytes.Equal(got, saved) {
		t.Error("a request past the next debounce window did not retry the write")
	}
}

// TestVerifyRefusesWhenTheStoreCannotBeRead: the login check reads the
// credential from the file, and a file it cannot read is a credential it does
// not know. Falling back to the copy in memory would accept whatever password
// that copy holds, which after a rotation is the one rotated away from.
func TestVerifyRefusesWhenTheStoreCannotBeRead(t *testing.T) {
	a, path, _, _ := runningBridge(t)
	saved := damage(t, path)

	if err := a.Verify("admin", leakedPassword); err == nil {
		t.Fatal("Verify accepted a password with the store file unreadable")
	}

	if err := os.WriteFile(path, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Verify("admin", leakedPassword); err != nil {
		t.Errorf("Verify still refuses once the file reads again: %v", err)
	}
}

// TestADeletedStoreFileIsNoCredential: the file is the credential, so a
// running bridge whose file is gone has none. It refuses logins rather than
// accept the password it loaded at start, and a session write does not put
// that password back on disk: whoever removed the file, or moved it aside,
// meant it gone.
func TestADeletedStoreFileIsNoCredential(t *testing.T) {
	a, path, session, clock := runningBridge(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	// The write first, so it cannot pass on a credential the login check
	// below already re-read.
	clock.t = clock.t.Add(time.Second)
	if _, err := a.ValidateSession(session); err != nil {
		t.Fatal(err)
	}
	if err := a.FlushSessions(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a session write recreated the deleted store file (stat err=%v)", err)
	}
	if err := a.Verify("admin", leakedPassword); !errors.Is(err, ErrNotInitialised) {
		t.Errorf("Verify with the store file gone = %v, want ErrNotInitialised", err)
	}
}

// TestAnInitialCredentialIsNotWrittenOverAnother: the credential writers
// decide "is there an account, and whose" from the file at the write, not
// from what was there at open. Two processes that both opened an empty store
// (`bridge init` and a bridge seeding from its environment, say) otherwise
// both mint, and the second silently replaces the password the first one
// printed; a reset-password under another name replaces the account.
//
// Each row is a store of its own, opened empty, so none can pass on an
// account an earlier row's read already put in its memory.
func TestAnInitialCredentialIsNotWrittenOverAnother(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adminauth.json")
	openEmpty := func() *Store {
		s, err := OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	mint, seed, rename := openEmpty(), openEmpty(), openEmpty()
	if err := openEmpty().SetInitialPassword("admin", leakedPassword); err != nil {
		t.Fatal(err)
	}

	if _, err := mint.MintInitial("admin"); !errors.Is(err, ErrAlreadyInitialised) {
		t.Errorf("MintInitial over a store another process initialised = %v, want ErrAlreadyInitialised", err)
	}
	if err := seed.SetInitialPassword("admin", rotatedPassword); !errors.Is(err, ErrAlreadyInitialised) {
		t.Errorf("SetInitialPassword over a store another process initialised = %v, want ErrAlreadyInitialised", err)
	}
	if err := rename.ResetPassword("someone-else", rotatedPassword); !errors.Is(err, ErrUsernameMismatch) {
		t.Errorf("ResetPassword under another name over a store another process initialised = %v, want ErrUsernameMismatch", err)
	}
	c, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Verify("admin", leakedPassword); err != nil {
		t.Errorf("the first credential was replaced: %v", err)
	}
}

// TestARotationLeavesLoginTicketsAlone: rotating the password writes the
// credential file and nothing beside it. A login link minted before the
// rotation still redeems, and its sidecar is byte for byte what it was.
func TestARotationLeavesLoginTicketsAlone(t *testing.T) {
	a, path, _, _ := runningBridge(t)
	cli, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := cli.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(a.ticketPath())
	if err != nil {
		t.Fatal(err)
	}

	rotateElsewhere(t, path)

	if after, err := os.ReadFile(a.ticketPath()); err != nil || !bytes.Equal(before, after) {
		t.Errorf("the rotation changed the login-ticket file (err=%v)", err)
	}
	if user, err := a.RedeemLoginTicket(ticket); err != nil || user != "admin" {
		t.Errorf("a ticket minted before the rotation = (%q, %v), want (admin, nil)", user, err)
	}
}

// TestRedemptionRefusesAnAccountReplacedElsewhere: a ticket names an account,
// and the running bridge checks that account against the FILE, since the
// process that replaces an account is never the one redeeming. Mint and redeem
// happen in different processes up to MaxLoginTicketTTL apart, and
// CreateSession validates nothing, so a check against the account loaded at
// start mints a session for an account the store no longer has.
func TestRedemptionRefusesAnAccountReplacedElsewhere(t *testing.T) {
	a, path, _, _ := runningBridge(t)
	ticket, err := a.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}

	// The account is replaced by another process: the store moved aside and a
	// new one made under another name.
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

	if user, err := a.RedeemLoginTicket(ticket); !errors.Is(err, ErrTicketInvalid) {
		t.Errorf("a ticket for an account replaced elsewhere = (%q, %v), want ErrTicketInvalid", user, err)
	}
}

// TestMintRefusesAnAccountReplacedElsewhere is the mint half of the check
// above: `bridge admin login-link` names an account, and the store answers
// from the file rather than from what it read at open.
func TestMintRefusesAnAccountReplacedElsewhere(t *testing.T) {
	_, path, _, _ := runningBridge(t)
	cli, err := OpenStore(path)
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

	if _, err := cli.MintLoginTicket("admin"); err == nil {
		t.Error("a ticket was minted for an account the file no longer has")
	}
	if _, err := cli.MintLoginTicket("someone-else"); err != nil {
		t.Errorf("a ticket for the account the file does have was refused: %v", err)
	}
}

// TestRedemptionThatCannotReadTheStoreDoesNotSpendTheTicket: a redemption that
// cannot read the credential has established nothing about the ticket, so it
// leaves the ticket on disk, as a ticket file that cannot be written does. The
// admin handler answers that error with a 500 that says the record is still
// there, and the link works once the file reads again.
func TestRedemptionThatCannotReadTheStoreDoesNotSpendTheTicket(t *testing.T) {
	a, path, _, _ := runningBridge(t)
	ticket, err := a.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	saved := damage(t, path)

	if _, err := a.RedeemLoginTicket(ticket); err == nil || errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("redemption with the store unreadable = %v, want an error that is not ErrTicketInvalid", err)
	}

	if err := os.WriteFile(path, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	if user, err := a.RedeemLoginTicket(ticket); err != nil || user != "admin" {
		t.Errorf("the ticket after the file reads again = (%q, %v), want (admin, nil): "+
			"the failed redemption spent it", user, err)
	}
}
