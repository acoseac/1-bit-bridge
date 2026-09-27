package adminauth

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRateLimiterStopIsSafeUnderConcurrentCallers pins the
// CodeRabbit Major fix on PR #292: Stop() must be safe to call
// from any number of goroutines concurrently. Pre-fix the
// select/default + bare close pattern could panic on double-
// close. sync.Once gates the close; everyone waits on
// `<-done` after.
//
// `-race` is the load-bearing checker — even without an outright
// panic, the race detector flags concurrent close+close on the
// same channel. This test runs under `go test -race` in CI.
func TestRateLimiterStopIsSafeUnderConcurrentCallers(t *testing.T) {
	rl := NewRateLimiter()

	const callers = 32
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rl.Stop()
		}()
	}
	wg.Wait()
	// All Stop() calls returned without panic + the janitor
	// goroutine exited (done is closed at the top of runJanitor's
	// defer).
	select {
	case <-rl.done:
		// expected — janitor exited
	default:
		t.Error("done channel should be closed after Stop() — janitor never exited")
	}
}

// TestResetPasswordRollsBackOnPersistFailure pins CodeRabbit's
// Major rollback finding on PR #292: a failed rotation must leave
// in-memory state matching disk, or the new password logs in until
// the next restart silently reverts it. ResetPassword reads the file
// before it writes it (the cross-process rule in the Store
// docblock), so there are two places to fail, one subtest each.
//
// The original fixture, a store path under a regular file, is used
// by neither: it now fails at the read, and on Windows that read
// answers ERROR_PATH_NOT_FOUND, which is os.ErrNotExist there, so it
// reads as "no store" rather than as an error. A damaged file fails
// the read on every platform.
func TestResetPasswordRollsBackOnPersistFailure(t *testing.T) {
	const original, rotated = "the original password", "new-password-XYZ"
	setup := func(t *testing.T) (s *Store, dir string, before userRecord) {
		t.Helper()
		dir = t.TempDir()
		s, err := OpenStore(filepath.Join(dir, "adminauth.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetInitialPassword("admin", original); err != nil {
			t.Fatal(err)
		}
		return s, dir, *s.user
	}
	requireUnchanged := func(t *testing.T, s *Store, before userRecord) {
		t.Helper()
		// In-memory state must reflect the ORIGINAL record, not the
		// (failed) new one.
		if s.user == nil {
			t.Fatal("s.user is nil after the failed rotation — it should be the original")
		}
		if s.user.PasswordHash != before.PasswordHash {
			t.Errorf("PasswordHash leaked through the failure: got %q, want the original %q",
				s.user.PasswordHash, before.PasswordHash)
		}
		if !s.user.PasswordChangedAt.Equal(before.PasswordChangedAt) {
			t.Errorf("PasswordChangedAt diverged: got %v, want %v",
				s.user.PasswordChangedAt, before.PasswordChangedAt)
		}
		if !s.user.CreatedAt.Equal(before.CreatedAt) {
			t.Errorf("CreatedAt diverged: got %v, want %v",
				s.user.CreatedAt, before.CreatedAt)
		}
	}

	t.Run("the read fails", func(t *testing.T) {
		s, _, before := setup(t)
		damaged := []byte(`{"user": `)
		if err := os.WriteFile(s.path, damaged, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.ResetPassword("admin", rotated); err == nil {
			t.Fatal("expected a failure (the file does not parse), got nil")
		}
		requireUnchanged(t, s, before)
		// Nor is a file it could not read written over: it may hold a
		// credential newer than any this process has seen.
		if got, _ := os.ReadFile(s.path); string(got) != string(damaged) {
			t.Errorf("a rotation that could not read the store wrote over it:\n%s", got)
		}
	})

	t.Run("the write fails", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows ignores a directory's read-only attribute when creating a file in it")
		}
		if os.Geteuid() == 0 {
			t.Skip("root creates files in a directory whatever its mode")
		}
		s, dir, before := setup(t)
		// Readable, so the read succeeds, and not writable, so staging
		// the new file fails.
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if err := s.ResetPassword("admin", rotated); err == nil {
			t.Fatal("expected a write failure (directory not writable), got nil")
		}
		requireUnchanged(t, s, before)
		if err := s.Verify("admin", rotated); err == nil {
			t.Error("the new password verifies after a rotation that never reached the file")
		}
		if err := s.Verify("admin", original); err != nil {
			t.Errorf("the original password no longer verifies: %v", err)
		}
	})
}

// TestAnInitialCredentialWhoseWriteFailsIsNotLive is the rollback pin
// for the first credential. A credential that never reached the file
// must not be live in memory: the next restart would not have it, so an
// operator would be locked out by a bridge that had just accepted them.
func TestAnInitialCredentialWhoseWriteFailsIsNotLive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ignores a directory's read-only attribute when creating a file in it")
	}
	if os.Geteuid() == 0 {
		t.Skip("root creates files in a directory whatever its mode")
	}
	for _, tc := range []struct {
		name string
		mint func(s *Store) error
	}{
		{"SetInitialPassword", func(s *Store) error { return s.SetInitialPassword("admin", "an initial password") }},
		{"MintInitial", func(s *Store) error { _, err := s.MintInitial("admin"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := OpenStore(filepath.Join(dir, "adminauth.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			if err := tc.mint(s); err == nil {
				t.Fatal("expected a write failure (directory not writable), got nil")
			}
			if s.IsInitialised() {
				t.Error("a credential that never reached the file is live in memory")
			}
		})
	}
}

// TestResetPasswordBuildsNewPointer pins the race-safety
// contract from the CodeRabbit Critical review: ResetPassword
// MUST build a fresh *userRecord and swap, not mutate in place.
// A `Verify` that captured the old pointer pre-swap continues
// reading the old PasswordHash through its bcrypt call —
// no data race.
//
// Drive `-race`-observable concurrency by running many parallel
// ResetPasswords + Verifys; the race detector flags any
// concurrent write to the same memory the read path touches.
func TestResetPasswordBuildsNewPointer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adminauth.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	pw, err := s.MintInitial("admin")
	if err != nil {
		t.Fatal(err)
	}

	const verifies = 16
	const resets = 8
	var verifiesDone, resetsDone atomic.Int32
	stopVerify := make(chan struct{})
	// Collect ResetPassword errors via a buffered channel so a
	// race-window persist failure surfaces as a test failure
	// rather than passing silently (CodeRabbit Minor review
	// post-PR-#296 — the pre-fix `_ = s.ResetPassword(...)`
	// would let a no-reset-succeeded run mark the test green
	// while the pointer-swap contract was actually unexercised).
	resetErrs := make(chan error, resets)

	var wg sync.WaitGroup
	// Resets first — bounded count, so we know when to stop the
	// verifies.
	for i := 0; i < resets; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer resetsDone.Add(1)
			if err := s.ResetPassword("admin", "newpw-"+pw); err != nil {
				resetErrs <- err
			}
		}()
	}
	// Verifies — run until all resets complete.
	for i := 0; i < verifies; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer verifiesDone.Add(1)
			for {
				select {
				case <-stopVerify:
					return
				default:
				}
				// Either result is legitimate — the race
				// detector cares about the read, not the
				// outcome.
				_ = s.Verify("admin", pw)
			}
		}()
	}
	// Wait for resets to finish, then stop verifies.
	for resetsDone.Load() < resets {
		time.Sleep(time.Millisecond)
	}
	close(stopVerify)
	wg.Wait()
	close(resetErrs)
	for err := range resetErrs {
		t.Fatalf("ResetPassword failed during race-stress: %v", err)
	}
	if verifiesDone.Load() != verifies {
		t.Errorf("verifies finished = %d, want %d", verifiesDone.Load(), verifies)
	}
}

// TestSessionSurvivesResetPassword: an active session created
// BEFORE ResetPassword stays valid afterwards. Operator-friendly
// — rotating credentials from CLI doesn't kick the operator's
// active admin browser tab. The hard-cap + idle-timeout still
// govern; ResetPassword does not enumerate-and-invalidate
// sessions.
//
// This is the documented contract in store.go's ResetPassword
// docblock; pinning it here so a future refactor that decides
// to invalidate-on-reset gets caught.
func TestSessionSurvivesResetPassword(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adminauth.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MintInitial("admin"); err != nil {
		t.Fatal(err)
	}
	raw, err := s.CreateSession("admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResetPassword("admin", "fresh-password-1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ValidateSession(raw); err != nil {
		t.Errorf("session should survive ResetPassword; got %v", err)
	}
}
