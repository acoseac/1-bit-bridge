package adminauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The tests in this file follow a sign-out: every console session ended by a
// process that does not hold them. `bridge admin reset-password` (by default)
// and `bridge admin sign-out-everywhere` each open a store of their own beside
// the running bridge (`a`, from runningBridge), which holds the sessions in
// memory and writes them back, so a file that merely lost them ends nothing
// there. Until 2026-09-27 nothing could: a rotation kept them, a restart
// reloaded them, and a logout ends only the caller's own.

// signOutElsewhere is `bridge admin sign-out-everywhere` run against the file
// while the bridge runs.
func signOutElsewhere(t *testing.T, path string) {
	t.Helper()
	b, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SignOutEverywhere(); err != nil {
		t.Fatal(err)
	}
}

// rotateAndSignOutElsewhere is `bridge admin reset-password` with its default.
func rotateAndSignOutElsewhere(t *testing.T, path string) {
	t.Helper()
	b, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ResetPassword("admin", rotatedPassword, EndSessions); err != nil {
		t.Fatal(err)
	}
}

// requireEndedOnDisk opens the file as the NEXT start of the bridge would, and
// requires that none of sessions signs in there.
func requireEndedOnDisk(t *testing.T, path, after string, sessions ...string) {
	t.Helper()
	c, err := OpenStore(path)
	if err != nil {
		t.Fatalf("after %s, the store no longer opens: %v", after, err)
	}
	for i, raw := range sessions {
		if _, err := c.ValidateSession(raw); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("after %s, signed-out session %d is back in the file (err=%v): "+
				"a restart signs it in again", after, i, err)
		}
	}
}

// fileSignOut reads the sign-out marker straight out of the file, zero for
// none.
func fileSignOut(t *testing.T, path string) time.Time {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f storeFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.SessionsRevokedAt == nil {
		return time.Time{}
	}
	return *f.SessionsRevokedAt
}

// TestASignOutElsewhereEndsTheRunningBridgesSessionsAtTheirNextRequest is the
// gap this change closes: a console signed in with a leaked password outlived
// the rotation, since the running bridge held it in memory, went on
// validating it, and wrote it back at its next write, and a restart reloaded
// it. The check is the running store's FIRST act after the sign-out, with
// nothing of its own written in between, so only the session check itself
// can have read the sign-out.
func TestASignOutElsewhereEndsTheRunningBridgesSessionsAtTheirNextRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		signOut func(t *testing.T, path string)
		// password is the one the file must still accept afterwards.
		password string
	}{
		{"sign-out-everywhere", signOutElsewhere, leakedPassword},
		{"reset-password", rotateAndSignOutElsewhere, rotatedPassword},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, path, first, _ := runningBridge(t)
			second, err := a.CreateSession("admin")
			if err != nil {
				t.Fatal(err)
			}

			tc.signOut(t, path)

			for i, raw := range []string{first, second} {
				if _, err := a.ValidateSession(raw); !errors.Is(err, ErrSessionNotFound) {
					t.Errorf("session %d on the running bridge after %s = %v, want ErrSessionNotFound",
						i, tc.name, err)
				}
			}
			requireEndedOnDisk(t, path, tc.name, first, second)
			if err := a.Verify("admin", tc.password); err != nil {
				t.Errorf("after %s, the running bridge refuses the password it should accept: %v", tc.name, err)
			}
		})
	}
}

// TestAWriteByTheRunningBridgeDoesNotBringBackSignedOutSessions: every session
// write puts down the set the running bridge holds, so the sign-out must reach
// that set before the write builds it. Each row's write is the running store's
// first act after the sign-out, the rows of
// TestAWriteByTheRunningBridgeKeepsARotationMadeElsewhere for the sessions:
// built before the read, the set wrote every signed-out session back into the
// file the sign-out had emptied, for the next restart to sign in again.
func TestAWriteByTheRunningBridgeDoesNotBringBackSignedOutSessions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, a *Store, first, second string, clock *testClock)
		// ended are the sessions the running bridge held before the
		// sign-out and must hold none of afterwards.
		ended func(first, second string) []string
	}{
		{"a login",
			func(t *testing.T, a *Store, _, _ string, _ *testClock) {
				if _, err := a.CreateSession("admin"); err != nil {
					t.Fatal(err)
				}
			},
			func(first, second string) []string { return []string{first, second} }},
		{"a logout",
			func(t *testing.T, a *Store, _, second string, _ *testClock) {
				a.DeleteSession(second)
			},
			func(first, _ string) []string { return []string{first} }},
		{"the shutdown flush",
			func(t *testing.T, a *Store, _, _ string, _ *testClock) {
				if err := a.FlushSessions(); err != nil {
					t.Fatal(err)
				}
			},
			func(first, second string) []string { return []string{first, second} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, path, first, clock := runningBridge(t)
			second, err := a.CreateSession("admin")
			if err != nil {
				t.Fatal(err)
			}
			// Activity inside the debounce window, so the flush row has
			// something pending, from before the sign-out.
			clock.t = clock.t.Add(time.Second)
			if _, err := a.ValidateSession(first); err != nil {
				t.Fatal(err)
			}

			signOutElsewhere(t, path)
			tc.write(t, a, first, second, clock)

			ended := tc.ended(first, second)
			requireEndedOnDisk(t, path, tc.name, ended...)
			for i, raw := range ended {
				if _, err := a.ValidateSession(raw); !errors.Is(err, ErrSessionNotFound) {
					t.Errorf("after %s, the running bridge still holds signed-out session %d (err=%v)", tc.name, i, err)
				}
			}
		})
	}
}

// TestASessionMadeAfterASignOutIsKept: a sign-out ends the sessions that were
// signed in when the running bridge read it, and none made after. A login made
// after the read stays signed in through the bridge's own writes and a
// restart, although the file still holds the marker.
//
// The running bridge's clock is an hour BEHIND the marker, so the session's
// IssuedAt predates it: the marker is an event, taken once, never a filter on
// IssuedAt. A filter would end this session at its next check, and every
// session after a clock stepped back, until the clock passed the marker.
func TestASessionMadeAfterASignOutIsKept(t *testing.T) {
	a, path, _, clock := runningBridge(t)
	signOutElsewhere(t, path)
	clock.t = fileSignOut(t, path).Add(-time.Hour)

	// A login: the credential check reads the file, and takes the sign-out.
	if err := a.Verify("admin", leakedPassword); err != nil {
		t.Fatal(err)
	}
	after, err := a.CreateSession("admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ValidateSession(after); err != nil {
		t.Fatalf("a session made after the sign-out was refused: %v", err)
	}
	clock.t = clock.t.Add(sessionFlushInterval + time.Second)
	if _, err := a.ValidateSession(after); err != nil {
		t.Fatalf("a session made after the sign-out was refused past the debounce: %v", err)
	}
	if err := a.FlushSessions(); err != nil {
		t.Fatal(err)
	}
	// The next start: it opens the file, marker and all, and reads it again
	// at its first login attempt. It must take the marker it opened with as
	// one already taken, or that first read signs out every session it
	// loaded, at every start after a sign-out.
	c, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Verify("admin", leakedPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ValidateSession(after); err != nil {
		t.Errorf("a session made after the sign-out does not survive the next start: %v", err)
	}
}

// TestEverySignOutChangesTheMarker: a running bridge ignores a marker it has
// taken, so a second sign-out must write a different one, whatever the clock
// says. Both signing-out stores' clocks read the same instant here, which is
// two sign-outs inside one tick of a coarse clock (15.6 ms on Windows) or a
// clock stepped back between them: with the clock's time written as it is,
// the second marker equals the first, and the session signed in between them
// survives the second sign-out.
func TestEverySignOutChangesTheMarker(t *testing.T) {
	a, path, first, _ := runningBridge(t)
	frozen := time.Now().Round(0)
	signOutAt := func() {
		t.Helper()
		b, err := OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		b.now = func() time.Time { return frozen }
		if err := b.SignOutEverywhere(); err != nil {
			t.Fatal(err)
		}
	}

	signOutAt()
	if _, err := a.ValidateSession(first); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("the first sign-out did not reach the running bridge (err=%v)", err)
	}
	if err := a.Verify("admin", leakedPassword); err != nil {
		t.Fatal(err)
	}
	between, err := a.CreateSession("admin")
	if err != nil {
		t.Fatal(err)
	}
	marker := fileSignOut(t, path)

	signOutAt()
	if again := fileSignOut(t, path); again.Equal(marker) {
		t.Errorf("the second sign-out wrote the marker the first one did (%s)", again)
	}
	if _, err := a.ValidateSession(between); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("a session signed in between two sign-outs survived the second (err=%v)", err)
	}
}

// TestASignOutDuringASessionWriteIsNotUndone is
// TestARotationDuringASessionWriteIsNotUndone for a sign-out: the running
// bridge stages a session write, the sign-out commits inside its fsync, and
// the bridge's commit must not rename the sessions it staged over the file the
// sign-out emptied. It reads the file once more before its commit, and the
// rebuilt write takes the sign-out first.
func TestASignOutDuringASessionWriteIsNotUndone(t *testing.T) {
	a, path, first, _ := runningBridge(t)
	fired := false
	beforeCommitHook = func() {
		if !fired {
			fired = true
			signOutElsewhere(t, path)
		}
	}
	t.Cleanup(func() { beforeCommitHook = nil })

	signedIn, err := a.CreateSession("admin")
	if err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("the sign-out never ran: the session write did not reach its commit")
	}
	requireEndedOnDisk(t, path, "a sign-out landing inside a session write", first, signedIn)
	if _, err := a.ValidateSession(first); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("the running bridge still holds a signed-out session (err=%v)", err)
	}
}

// TestAWriteThatKeepsTheSessionsCarriesTheSignOut: every write carries the
// file's marker over, the credential writes included. A rotation run with
// --keep-sessions between a sign-out and the running bridge's next read of the
// file otherwise drops the marker, and the running bridge, never seeing it,
// keeps every session the sign-out ended in the file.
func TestAWriteThatKeepsTheSessionsCarriesTheSignOut(t *testing.T) {
	a, path, first, _ := runningBridge(t)
	signOutElsewhere(t, path)
	marker := fileSignOut(t, path)

	rotateElsewhere(t, path) // --keep-sessions
	if got := fileSignOut(t, path); !got.Equal(marker) {
		t.Fatalf("a rotation keeping the sessions wrote marker %s over %s", got, marker)
	}
	if _, err := a.ValidateSession(first); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("a sign-out followed by a rotation keeping the sessions never reached "+
			"the running bridge (err=%v)", err)
	}

	// And the running bridge's own writes carry it too.
	if _, err := a.CreateSession("admin"); err != nil {
		t.Fatal(err)
	}
	if got := fileSignOut(t, path); !got.Equal(marker) {
		t.Errorf("the running bridge's session write wrote marker %s over %s", got, marker)
	}
}

// TestTheSessionCheckReadsTheStoreOnlyWhenItChanged: the check runs on every
// console request, so it reads the file only when a stat says the file is not
// the one it last read, and then once.
func TestTheSessionCheckReadsTheStoreOnlyWhenItChanged(t *testing.T) {
	a, path, first, clock := runningBridge(t)
	reads := 0
	readStoreHook = func() { reads++ }
	t.Cleanup(func() { readStoreHook = nil })

	// The login's own write replaced the file after its read, so one read
	// settles the stamp.
	if _, err := a.ValidateSession(first); err != nil {
		t.Fatal(err)
	}
	reads = 0
	for i := 0; i < 50; i++ {
		clock.t = clock.t.Add(100 * time.Millisecond) // inside the debounce: no write of its own
		if _, err := a.ValidateSession(first); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 0 {
		t.Errorf("50 session checks of an unchanged store read it %d times, want 0", reads)
	}

	signOutElsewhere(t, path)
	if _, err := a.ValidateSession(first); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("the check after a sign-out = %v, want ErrSessionNotFound", err)
	}
	if reads != 1 {
		t.Errorf("the check after a sign-out read the store %d times, want 1", reads)
	}
}

// TestTheSessionCheckSeesAReplacementWithTheSameSizeAndTime: every writer
// here replaces the file by rename, so a new file is a new inode (file ID on
// Windows), and the stat gate compares that as well as the size and the
// modification time. A sign-out landing within one tick of a coarse
// filesystem clock, at the size of the file it replaces, is otherwise missed
// until the bridge's next write. The replacement here is built to match both.
func TestTheSessionCheckSeesAReplacementWithTheSameSizeAndTime(t *testing.T) {
	a, path, first, _ := runningBridge(t)
	if _, err := a.ValidateSession(first); err != nil {
		t.Fatal(err)
	}
	old, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// The file a sign-out writes, padded to the old one's size.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f storeFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Round(0)
	f.SessionsRevokedAt, f.Sessions = &at, nil
	next, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	pad := int(old.Size()) - len(next)
	if pad < 0 {
		t.Fatalf("the fixture's sign-out file (%d bytes) is larger than the file it replaces (%d)", len(next), old.Size())
	}
	next = append(next, bytes.Repeat([]byte(" "), pad)...)
	tmp := filepath.Join(filepath.Dir(path), "replacement.json")
	if err := os.WriteFile(tmp, next, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, old.ModTime(), old.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if now, err := os.Stat(path); err != nil || now.Size() != old.Size() || !now.ModTime().Equal(old.ModTime()) {
		t.Fatalf("the replacement does not match the old file's size and time (err=%v), so it tests nothing", err)
	}

	if _, err := a.ValidateSession(first); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("a sign-out renamed in at the old file's size and time was not seen (err=%v)", err)
	}
}

// TestASessionCheckThatCannotReadTheStoreRefusesAndKeepsTheSession: the file
// may hold a sign-out this process has not taken, so a check that cannot read
// it refuses the session, as a login check does. It keeps the session: once
// the file reads again, the session is whatever the file says. And it logs the
// read at most once a window, since every console request makes one.
func TestASessionCheckThatCannotReadTheStoreRefusesAndKeepsTheSession(t *testing.T) {
	a, path, first, clock := runningBridge(t)
	rec := loggingtest.Record(t)
	saved := damage(t, path)

	for i := 0; i < 3; i++ {
		clock.t = clock.t.Add(time.Second)
		if _, err := a.ValidateSession(first); !errors.Is(err, ErrStoreUnreadable) {
			t.Fatalf("check %d with the store unreadable = %v, want ErrStoreUnreadable", i, err)
		}
	}
	if got := rec.Failures(msgStoreUnreadableLog); len(got) != 1 {
		t.Errorf("three refused checks inside one window logged %d lines, want 1:\n%s",
			len(got), strings.Join(got, "\n"))
	}
	if got, _ := os.ReadFile(path); string(got) != `{"user": ` {
		t.Errorf("a refused check wrote over the store:\n%s", got)
	}

	if err := os.WriteFile(path, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ValidateSession(first); err != nil {
		t.Errorf("the session after the store reads again = %v, want it kept", err)
	}

	// Unreadable again past the window: a line again.
	damage(t, path)
	clock.t = clock.t.Add(sessionFlushInterval)
	if _, err := a.ValidateSession(first); !errors.Is(err, ErrStoreUnreadable) {
		t.Fatalf("the check with the store unreadable again = %v, want ErrStoreUnreadable", err)
	}
	if got := rec.Failures(msgStoreUnreadableLog); len(got) != 2 {
		t.Errorf("a refused check past the window logged %d lines in all, want 2", len(got))
	}
}

// TestSignOutEverywhereNeedsACredential: the marker is written beside the
// credential, and a store without one serves no console. The command refuses,
// and writes no file: a session write does not recreate a deleted one either.
func TestSignOutEverywhereNeedsACredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adminauth.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SignOutEverywhere(); !errors.Is(err, ErrNotInitialised) {
		t.Errorf("SignOutEverywhere on an empty store = %v, want ErrNotInitialised", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("SignOutEverywhere on an empty store wrote a file (stat err=%v)", err)
	}
}

// TestASignOutDoesNotWriteOverAStoreItCannotRead: the rule every writer
// follows (#1039). The file may hold a credential newer than any this process
// has seen, and a sign-out writes the file's credential back beside its
// marker.
func TestASignOutDoesNotWriteOverAStoreItCannotRead(t *testing.T) {
	_, path, _, _ := runningBridge(t)
	b, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	damage(t, path)
	if err := b.SignOutEverywhere(); err == nil {
		t.Fatal("SignOutEverywhere over a store it cannot read succeeded")
	}
	if got, _ := os.ReadFile(path); string(got) != `{"user": ` {
		t.Errorf("SignOutEverywhere wrote over a store it could not read:\n%s", got)
	}
}

// TestEndOtherSessionsKeepsOnlyTheCallers is the console's "Sign out all other
// sessions": every session but the caller's ends, in memory and in the file,
// and the count is of the live sessions it ended. It runs in the serving
// bridge, so it needs no marker.
func TestEndOtherSessionsKeepsOnlyTheCallers(t *testing.T) {
	a, path, caller, clock := runningBridge(t)
	stale, err := a.CreateSession("admin")
	if err != nil {
		t.Fatal(err)
	}
	// stale idles past the timeout while the caller stays active, so it is
	// already signed out and must not be counted.
	clock.t = clock.t.Add(SessionIdleTimeout - time.Hour)
	if _, err := a.ValidateSession(caller); err != nil {
		t.Fatal(err)
	}
	clock.t = clock.t.Add(2 * time.Hour)
	var others []string
	for i := 0; i < 2; i++ {
		raw, err := a.CreateSession("admin")
		if err != nil {
			t.Fatal(err)
		}
		others = append(others, raw)
	}
	if got := a.LiveSessionCount(); got != 3 {
		t.Fatalf("LiveSessionCount = %d, want 3 (the caller and two others; one expired)", got)
	}

	ended, err := a.EndOtherSessions(caller)
	if err != nil {
		t.Fatal(err)
	}
	if ended != 2 {
		t.Errorf("EndOtherSessions ended %d, want 2", ended)
	}
	if _, err := a.ValidateSession(caller); err != nil {
		t.Errorf("the caller's own session was ended: %v", err)
	}
	for i, raw := range others {
		if _, err := a.ValidateSession(raw); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("other session %d on the running bridge = %v, want ErrSessionNotFound", i, err)
		}
	}
	requireEndedOnDisk(t, path, "EndOtherSessions", append(others, stale)...)
	c, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	c.now = clock.now
	if _, err := c.ValidateSession(caller); err != nil {
		t.Errorf("the caller's session is not in the file for the next start: %v", err)
	}
}

// TestEndOtherSessionsNeedsALiveCaller: a token that names no session ends
// nothing, rather than every session including one it cannot name.
func TestEndOtherSessionsNeedsALiveCaller(t *testing.T) {
	a, _, first, _ := runningBridge(t)
	for _, raw := range []string{"", "not a session"} {
		if n, err := a.EndOtherSessions(raw); !errors.Is(err, ErrSessionNotFound) || n != 0 {
			t.Errorf("EndOtherSessions(%q) = (%d, %v), want (0, ErrSessionNotFound)", raw, n, err)
		}
	}
	if _, err := a.ValidateSession(first); err != nil {
		t.Errorf("a refused EndOtherSessions ended a session: %v", err)
	}
}
