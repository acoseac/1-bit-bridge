package adminauth

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The tests in this file open more than one store on one adminauth.json,
// which is how the credential is shared in production. `bridge serve` holds one for its
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
	return a, path, signIn(t, a, "admin", leakedPassword), clock
}

// rotateElsewhere is `bridge admin reset-password --keep-sessions` run against
// the file while the bridge runs: a store of its own, opened now, rotating the
// credential. It keeps the sessions because what the tests that use it follow
// is the credential, and a rotation that ends them leaves those tests nothing
// of the running bridge's to write; signoutelsewhere_test.go follows the end.
func rotateElsewhere(t *testing.T, path string) {
	t.Helper()
	b, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ResetPassword("admin", rotatedPassword, KeepSessions); err != nil {
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
	if _, err := c.Verify("admin", rotatedPassword); err != nil {
		t.Errorf("after %s, the file refuses the rotated password (%v): the running "+
			"bridge wrote the credential it held in memory back over the rotation", after, err)
	}
	if _, err := c.Verify("admin", leakedPassword); !errors.Is(err, ErrInvalidCredentials) {
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
//
// The login's is the one write the store refuses. Its check, made before the
// rotation, compared the password the rotation replaced, so its session is
// refused (Admission, backlog B881) and nothing is written over the rotation.
// Until B881 the row's login was CreateSession alone, which checked nothing,
// and its write landed beside the rotated credential.
func TestAWriteByTheRunningBridgeKeepsARotationMadeElsewhere(t *testing.T) {
	for _, tc := range []struct {
		name string
		// write is the running store's write. checked is a login's check made
		// before the rotation, which only the login uses.
		write func(t *testing.T, a *Store, session string, clock *testClock, checked Admission)
		// refused is a write the store must refuse, which leaves the file
		// byte for byte as the rotation wrote it.
		refused bool
	}{
		{"a login checked before the rotation", func(t *testing.T, a *Store, _ string, _ *testClock, checked Admission) {
			if _, err := a.CreateSession(checked); !errors.Is(err, ErrCredentialsChanged) {
				t.Fatalf("a login checked against the password the rotation replaced = %v, want ErrCredentialsChanged", err)
			}
		}, true},
		{"session activity past the debounce", func(t *testing.T, a *Store, session string, clock *testClock, _ Admission) {
			clock.t = clock.t.Add(sessionFlushInterval + time.Second)
			if _, err := a.ValidateSession(session); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"a logout", func(t *testing.T, a *Store, session string, _ *testClock, _ Admission) {
			a.DeleteSession(session)
		}, false},
		{"the shutdown flush", func(t *testing.T, a *Store, session string, clock *testClock, _ Admission) {
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
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, path, session, clock := runningBridge(t)
			checked := admit(t, a, "admin", leakedPassword)
			rotateElsewhere(t, path)
			rotated, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			tc.write(t, a, session, clock, checked)
			requireRotationOnDisk(t, path, tc.name)
			got, _ := os.ReadFile(path)
			if tc.refused {
				if !bytes.Equal(got, rotated) {
					t.Errorf("after %s, which the store refused, the file is not what the rotation wrote", tc.name)
				}
				return
			}
			// And the write itself landed. A write that only refused to
			// commit would keep the rotation too, while the running
			// bridge's sessions never reached the file again.
			if bytes.Equal(got, rotated) {
				t.Errorf("after %s, the file is exactly what the rotation wrote: the running "+
					"bridge's write never landed", tc.name)
			}
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

	if _, err := a.Verify("admin", leakedPassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("the running bridge still accepts the leaked password after a rotation (err=%v)", err)
	}
	if _, err := a.Verify("admin", rotatedPassword); err != nil {
		t.Errorf("the running bridge refuses the rotated password: %v", err)
	}
}

// TestARotationCarriesTheRunningBridgesSessions: a rotation that keeps the
// sessions (`--keep-sessions`) writes the file's session set as it finds it at
// the write, never the copy it read at open. `bridge admin reset-password`
// waits at a password prompt between the two, and the bridge goes on signing
// sessions in and out meanwhile: a session signed in during the prompt was
// dropped from the file, and one signed OUT during it was written back, a
// logout undone by the next restart.
//
// A rotation that ends them (the default) writes none, and
// signoutelsewhere_test.go follows that.
func TestARotationCarriesTheRunningBridgesSessions(t *testing.T) {
	a, path, signedOut, _ := runningBridge(t)
	b, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// While b waits at its prompt.
	signedIn := signIn(t, a, "admin", leakedPassword)
	a.DeleteSession(signedOut)
	if err := b.ResetPassword("admin", rotatedPassword, KeepSessions); err != nil {
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

// TestARotationDuringASessionWriteIsNotUndone: re-reading the credential
// before a session write is not enough on its own, because staging the file
// costs a write and an fsync, milliseconds (tens of them on a cloud disk) in
// which reset-password can land its rotation, and committing a file built
// from the credential read before them puts the old one back. The write
// reads the credential once more before its commit and rebuilds around a
// change, keeping the change it was writing.
//
// A login is the write that does not keep its change: its check compared the
// password the rotation replaced, and the rebuilt write's read is where its
// session is admitted, so it is refused there (Admission, backlog B881). Until
// B881 the login was CreateSession alone, and its rebuilt write landed it
// beside the rotated credential.
func TestARotationDuringASessionWriteIsNotUndone(t *testing.T) {
	for _, tc := range []struct {
		name string
		// write is the session write the rotation lands inside, by the
		// running bridge a, which also holds second; checked is a login's
		// check made before the write began.
		write func(t *testing.T, a *Store, second string, checked Admission)
		// held reports whether the reopened store c shows the write's change.
		held func(c *Store, second string) bool
	}{
		{"a logout",
			func(t *testing.T, a *Store, second string, _ Admission) { a.DeleteSession(second) },
			func(c *Store, second string) bool {
				_, err := c.ValidateSession(second)
				return errors.Is(err, ErrSessionNotFound)
			}},
		{"a login, refused",
			func(t *testing.T, a *Store, _ string, checked Admission) {
				if _, err := a.CreateSession(checked); !errors.Is(err, ErrCredentialsChanged) {
					t.Errorf("a login whose write found a rotation on its read again = %v, want ErrCredentialsChanged", err)
				}
				if n := a.SessionCount(); n != 2 {
					t.Errorf("the running bridge holds %d sessions after the refused login, want its 2", n)
				}
			},
			func(c *Store, _ string) bool { return c.SessionCount() == 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, path, _, _ := runningBridge(t)
			second := signIn(t, a, "admin", leakedPassword)
			checked := admit(t, a, "admin", leakedPassword)
			fired := false
			beforeCommitHook = func() {
				if !fired {
					fired = true
					rotateElsewhere(t, path)
				}
			}
			t.Cleanup(func() { beforeCommitHook = nil })

			tc.write(t, a, second, checked)
			if !fired {
				t.Fatal("the rotation never ran: the session write did not reach its commit")
			}
			requireRotationOnDisk(t, path, "a rotation landing inside "+tc.name)
			c, err := OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.held(c, second) {
				t.Errorf("the file after %s, whose write was rebuilt around the rotation, does not show it", tc.name)
			}
		})
	}
}

// TestARotationDoesNotCommitOverASessionChangeMadeDuringItsWrite is the same
// window from the other side (CodeRabbit on #1039). reset-password reads the
// file's sessions and then stages its write, and the running bridge commits
// logins and logouts meanwhile; renaming the staged file dropped the login
// and brought the logout back, and a logout brought back that way returns
// at the next restart, when nothing in the bridge's memory overrules it.
// The reset re-reads the file before its rename and rebuilds from it.
func TestARotationDoesNotCommitOverASessionChangeMadeDuringItsWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		// change is the running bridge's session change, made while the
		// rotation is staging; it returns the session it changed.
		change func(t *testing.T, a *Store, session string) string
		// kept reports whether a store opened afterwards shows the change.
		kept func(c *Store, session string) bool
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
				// Checked and admitted before the rotation lands: it reads
				// the file as it was, which the rotation has not replaced.
				return signIn(t, a, "admin", leakedPassword)
			},
			func(c *Store, session string) bool {
				_, err := c.ValidateSession(session)
				return err == nil
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, path, session, _ := runningBridge(t)
			var changed string
			fired := false
			beforeCommitHook = func() {
				if !fired {
					fired = true
					changed = tc.change(t, a, session)
				}
			}
			t.Cleanup(func() { beforeCommitHook = nil })

			rotateElsewhere(t, path)
			if !fired {
				t.Fatal("the session change never ran: the rotation did not reach its commit")
			}
			requireRotationOnDisk(t, path, tc.name+" during the rotation's write")
			c, err := OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.kept(c, changed) {
				t.Errorf("%s the running bridge committed while the rotation was writing "+
					"was overwritten by the rotation's commit", tc.name)
			}
		})
	}
}

// TestSessionsHeldWhileTheStoreIsGoneLandOnceItIsBack: with the file gone a
// session write has no credential to write beside, so it writes nothing, and
// the sessions stay pending rather than being dropped from the flush: they
// are the running bridge's, and go down beside the next credential the file
// holds.
func TestSessionsHeldWhileTheStoreIsGoneLandOnceItIsBack(t *testing.T) {
	a, path, session, clock := runningBridge(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	clock.t = clock.t.Add(time.Second)
	if _, err := a.ValidateSession(session); err != nil {
		t.Fatal(err)
	}
	if err := a.FlushSessions(); err != nil {
		t.Fatal(err)
	}

	// A credential is set again, from another process.
	rotateElsewhere(t, path)
	if err := a.FlushSessions(); err != nil {
		t.Fatal(err)
	}
	c, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ValidateSession(session); err != nil {
		t.Errorf("the running bridge's session never reached the file once a credential was back: %v", err)
	}
	requireRotationOnDisk(t, path, "the flush after a credential was set again")
}

// TestAWriteThatCannotReadTheStoreDoesNotOverwriteIt: a session write that
// cannot re-read the file writes nothing, because the file may hold a
// credential newer than the one in memory, and the change it held back lands
// once the file reads again, at the latest at the shutdown flush. A
// logout is the case where both halves matter: overwriting undoes a rotation,
// and dropping it undoes the logout at the next restart.
//
// A login was a row here until backlog B881, held in memory and landed by the
// flush. Its session is admitted on the read its write is built from now, so
// a login the file cannot be read for is refused and holds nothing:
// TestALoginTheFileCannotBeReadForIsRefused.
func TestAWriteThatCannotReadTheStoreDoesNotOverwriteIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// write makes the change while the file cannot be read, and returns
		// the session whose state it changed.
		write func(t *testing.T, a *Store, session string) string
		// landed reports whether a store shows the change: one opened
		// afterwards, and the one that made it.
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
			// And the process that made the change held it all along.
			if !tc.landed(a, changed) {
				t.Errorf("%s did not hold in the process that made it", tc.name)
			}
		})
	}
}

// TestAFailedSessionWriteWaitsOutTheDebounce: a session write that fails
// starts the next debounce window as a successful one does. Otherwise every
// authenticated request after the failure is "due", and a write that keeps
// failing costs an attempt and an error line per console request, which is
// the log flood the debounce exists to bound.
//
// The write fails because the file changes under every attempt, so each
// commit refuses and the last gives up (maxCommitAttempts). Until 2026-09-27
// the fixture was a file that could not be read, which now fails the session
// check itself, before the write; that case and its log bound are
// TestASessionCheckThatCannotReadTheStoreRefusesAndKeepsTheSession. An
// unwritable directory would fail the write too, on every platform but
// Windows, which ignores a directory's read-only attribute.
func TestAFailedSessionWriteWaitsOutTheDebounce(t *testing.T) {
	a, path, session, clock := runningBridge(t)
	attempts := 0
	beforeCommitHook = func() {
		attempts++
		touchInPlace(t, path)
	}
	t.Cleanup(func() { beforeCommitHook = nil })

	clock.t = clock.t.Add(sessionFlushInterval + time.Second)
	if _, err := a.ValidateSession(session); err != nil {
		t.Fatal(err)
	}
	if attempts != maxCommitAttempts {
		t.Fatalf("the due write made %d attempts, want %d: the fixture did not fail it", attempts, maxCommitAttempts)
	}
	beforeCommitHook = nil
	settled, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	clock.t = clock.t.Add(time.Second)
	if _, err := a.ValidateSession(session); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, settled) {
		t.Error("the request after a failed write wrote again, inside the debounce window")
	}

	clock.t = clock.t.Add(sessionFlushInterval)
	if _, err := a.ValidateSession(session); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); bytes.Equal(got, settled) {
		t.Error("a request past the next debounce window did not retry the write")
	}
}

// touchInPlace rewrites the store file with one more byte of trailing
// whitespace, which parses as the same store and is a different file to a
// commit's byte comparison: another process's write, for a test to land
// inside a commit.
func touchInPlace(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyRefusesWhenTheStoreCannotBeRead: the login check reads the
// credential from the file, and a file it cannot read is a credential it does
// not know. Falling back to the copy in memory would accept whatever password
// that copy holds, which after a rotation is the one rotated away from.
func TestVerifyRefusesWhenTheStoreCannotBeRead(t *testing.T) {
	a, path, _, _ := runningBridge(t)
	saved := damage(t, path)

	if _, err := a.Verify("admin", leakedPassword); err == nil {
		t.Fatal("Verify accepted a password with the store file unreadable")
	}

	if err := os.WriteFile(path, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Verify("admin", leakedPassword); err != nil {
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
	if _, err := a.Verify("admin", leakedPassword); !errors.Is(err, ErrNotInitialised) {
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
	if err := rename.ResetPassword("someone-else", rotatedPassword, EndSessions); !errors.Is(err, ErrUsernameMismatch) {
		t.Errorf("ResetPassword under another name over a store another process initialised = %v, want ErrUsernameMismatch", err)
	}
	c, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Verify("admin", leakedPassword); err != nil {
		t.Errorf("the first credential was replaced: %v", err)
	}
}

// TestARotationLeavesLoginTicketsAlone: rotating the password writes the
// credential file and nothing beside it. A login link minted before the
// rotation still redeems, and its file is byte for byte what it was.
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
	ticketPath := a.ticketFilePath(hashTicket(ticket))
	before, err := os.ReadFile(ticketPath)
	if err != nil {
		t.Fatal(err)
	}

	rotateElsewhere(t, path)

	if after, err := os.ReadFile(ticketPath); err != nil || !bytes.Equal(before, after) {
		t.Errorf("the rotation changed the login ticket's file (err=%v)", err)
	}
	if user, err := redeemed(a.RedeemLoginTicket(ticket)); err != nil || user != "admin" {
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

	if user, err := redeemed(a.RedeemLoginTicket(ticket)); !errors.Is(err, ErrTicketInvalid) {
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
// leaves the ticket on disk, as a ticket file that cannot be removed does. The
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
	if user, err := redeemed(a.RedeemLoginTicket(ticket)); err != nil || user != "admin" {
		t.Errorf("the ticket after the file reads again = (%q, %v), want (admin, nil): "+
			"the failed redemption spent it", user, err)
	}
}
