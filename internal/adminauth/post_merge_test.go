package adminauth

import (
	"os"
	"path/filepath"
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
// Major rollback finding on PR #292: persist() failure must
// leave in-memory state matching disk. Drive a persist failure
// by pointing the store at a path whose parent is a regular
// file (not a directory) — os.MkdirAll in persist() fails with
// `not a directory`. Verify the in-memory user record reverts
// to the pre-PATCH state instead of carrying the new hash.
func TestResetPasswordRollsBackOnPersistFailure(t *testing.T) {
	dir := t.TempDir()
	originalPath := filepath.Join(dir, "adminauth.json")
	s, err := OpenStore(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MintInitial("admin"); err != nil {
		t.Fatal(err)
	}
	originalHash := s.user.PasswordHash
	originalChangedAt := s.user.PasswordChangedAt
	originalCreatedAt := s.user.CreatedAt

	// Sabotage persist: redirect the store path to a child of a
	// regular file. os.MkdirAll inside persist sees the parent
	// is a file and fails with `not a directory`.
	conflictFile := filepath.Join(dir, "blocker")
	if err := os.WriteFile(conflictFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(conflictFile, "adminauth.json")

	if err := s.ResetPassword("admin", "new-password-XYZ"); err == nil {
		t.Fatal("expected persist failure (parent is a file, not a dir), got nil")
	}

	// In-memory state must reflect the ORIGINAL record, not the
	// (failed) new one.
	if s.user == nil {
		t.Fatal("s.user is nil after rollback — should have restored prev")
	}
	if s.user.PasswordHash != originalHash {
		t.Errorf("PasswordHash leaked through persist failure: got %q, want original %q",
			s.user.PasswordHash, originalHash)
	}
	if !s.user.PasswordChangedAt.Equal(originalChangedAt) {
		t.Errorf("PasswordChangedAt diverged: got %v, want %v",
			s.user.PasswordChangedAt, originalChangedAt)
	}
	if !s.user.CreatedAt.Equal(originalCreatedAt) {
		t.Errorf("CreatedAt diverged: got %v, want %v",
			s.user.CreatedAt, originalCreatedAt)
	}

	// Verify against the ORIGINAL password — must still work
	// because the rollback put back the original hash. (We don't
	// know the random plaintext from MintInitial; use bcrypt
	// against the in-memory hash to confirm it's the original
	// not the new one. Indirect: verify the new password is
	// REJECTED.)
	if err := s.Verify("admin", "new-password-XYZ"); err == nil {
		t.Error("new password should be rejected after rollback — the swap was undone")
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
	pw, _ := s.MintInitial("admin")

	const verifies = 16
	const resets = 8
	var verifiesDone, resetsDone atomic.Int32
	stopVerify := make(chan struct{})

	var wg sync.WaitGroup
	// Resets first — bounded count, so we know when to stop the
	// verifies.
	for i := 0; i < resets; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer resetsDone.Add(1)
			_ = s.ResetPassword("admin", "newpw-"+pw)
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
