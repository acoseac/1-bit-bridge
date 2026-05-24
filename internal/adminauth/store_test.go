package adminauth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "adminauth.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAdminBcryptCostIsAtLeast12(t *testing.T) {
	// Compile-time-style guard via runtime assertion: the
	// adminBcryptCost constant must stay at 12+ so brute-force
	// resistance doesn't silently regress.
	if adminBcryptCost < 12 {
		t.Fatalf("adminBcryptCost = %d, must be >= 12", adminBcryptCost)
	}
}

func TestMintInitialThenVerifyRoundTrip(t *testing.T) {
	s := newStore(t)
	if s.IsInitialised() {
		t.Fatal("fresh store should report uninitialised")
	}
	plaintext, err := s.MintInitial("admin")
	if err != nil {
		t.Fatalf("MintInitial: %v", err)
	}
	if plaintext == "" {
		t.Fatal("MintInitial returned empty plaintext")
	}
	if len(plaintext) < 12 {
		t.Errorf("plaintext too short: %d chars", len(plaintext))
	}
	if !s.IsInitialised() {
		t.Fatal("store still reports uninitialised after MintInitial")
	}
	if s.Username() != "admin" {
		t.Errorf("Username = %q, want %q", s.Username(), "admin")
	}
	if err := s.Verify("admin", plaintext); err != nil {
		t.Errorf("Verify with returned plaintext: %v", err)
	}
}

func TestMintInitialIdempotencyError(t *testing.T) {
	s := newStore(t)
	if _, err := s.MintInitial("admin"); err != nil {
		t.Fatalf("first MintInitial: %v", err)
	}
	_, err := s.MintInitial("admin")
	if !errors.Is(err, ErrAlreadyInitialised) {
		t.Errorf("second MintInitial: got %v, want ErrAlreadyInitialised", err)
	}
}

func TestVerifyRejectsWrongPassword(t *testing.T) {
	s := newStore(t)
	if _, err := s.MintInitial("admin"); err != nil {
		t.Fatal(err)
	}
	err := s.Verify("admin", "definitely-not-the-password")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Verify wrong password: got %v, want ErrInvalidCredentials", err)
	}
}

func TestVerifyRejectsWrongUsername(t *testing.T) {
	s := newStore(t)
	plaintext, _ := s.MintInitial("admin")
	err := s.Verify("attacker", plaintext)
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Verify wrong username: got %v, want ErrInvalidCredentials", err)
	}
}

func TestVerifyOnUninitialisedReturnsNotInitialised(t *testing.T) {
	s := newStore(t)
	err := s.Verify("admin", "any")
	if !errors.Is(err, ErrNotInitialised) {
		t.Errorf("Verify uninitialised: got %v, want ErrNotInitialised", err)
	}
}

func TestPasswordPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adminauth.json")
	s1, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := s1.MintInitial("admin")
	if err != nil {
		t.Fatal(err)
	}

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.IsInitialised() {
		t.Fatal("reopened store should be initialised")
	}
	if err := s2.Verify("admin", plaintext); err != nil {
		t.Errorf("Verify on reopened store: %v", err)
	}
}

func TestResetPasswordChangesHashAndKeepsUsername(t *testing.T) {
	s := newStore(t)
	old, _ := s.MintInitial("admin")
	if err := s.ResetPassword("admin", "new-strong-pw-XYZ"); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify("admin", old); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Verify with old password after reset: got %v, want ErrInvalidCredentials", err)
	}
	if err := s.Verify("admin", "new-strong-pw-XYZ"); err != nil {
		t.Errorf("Verify with new password: %v", err)
	}
}

func TestResetPasswordRejectsUsernameMismatch(t *testing.T) {
	s := newStore(t)
	if _, err := s.MintInitial("admin"); err != nil {
		t.Fatal(err)
	}
	err := s.ResetPassword("alice", "new-pw")
	if !errors.Is(err, ErrUsernameMismatch) {
		t.Errorf("ResetPassword with wrong username: got %v, want ErrUsernameMismatch", err)
	}
}

func TestResetPasswordRejectsEmptyPassword(t *testing.T) {
	s := newStore(t)
	_, _ = s.MintInitial("admin")
	if err := s.ResetPassword("admin", ""); err == nil {
		t.Error("ResetPassword with empty password: expected error, got nil")
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := newStore(t)
	_, _ = s.MintInitial("admin")

	raw, err := s.CreateSession("admin")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 32 {
		t.Errorf("session token too short: %d chars", len(raw))
	}

	got, err := s.ValidateSession(raw)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if got.Username != "admin" {
		t.Errorf("session.Username = %q, want %q", got.Username, "admin")
	}

	if s.SessionCount() != 1 {
		t.Errorf("SessionCount = %d, want 1", s.SessionCount())
	}

	s.DeleteSession(raw)
	if s.SessionCount() != 0 {
		t.Errorf("after Delete, SessionCount = %d, want 0", s.SessionCount())
	}
	if _, err := s.ValidateSession(raw); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Validate post-delete: got %v, want ErrSessionNotFound", err)
	}
}

func TestSessionIdleTimeoutExpires(t *testing.T) {
	s := newStore(t)
	_, _ = s.MintInitial("admin")
	// Inject a controllable clock.
	tick := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return tick }

	raw, _ := s.CreateSession("admin")
	if _, err := s.ValidateSession(raw); err != nil {
		t.Fatalf("Validate immediately after create: %v", err)
	}

	// Advance past the idle timeout WITHOUT touching the session.
	tick = tick.Add(SessionIdleTimeout + time.Minute)
	if _, err := s.ValidateSession(raw); !errors.Is(err, ErrSessionExpired) {
		t.Errorf("Validate past idle timeout: got %v, want ErrSessionExpired", err)
	}
}

func TestSessionHardCapExpires(t *testing.T) {
	s := newStore(t)
	_, _ = s.MintInitial("admin")
	tick := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return tick }

	raw, _ := s.CreateSession("admin")

	// Keep the session "active" by validating just under the idle
	// timeout, but cross the hard cap.
	for tick.Sub(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)) < SessionHardCap+time.Minute {
		_, _ = s.ValidateSession(raw)
		tick = tick.Add(SessionIdleTimeout - time.Hour)
	}
	if _, err := s.ValidateSession(raw); !errors.Is(err, ErrSessionExpired) {
		t.Errorf("Validate past hard cap: got %v, want ErrSessionExpired", err)
	}
}

func TestConcurrentLoginsDoNotRace(t *testing.T) {
	// Smoke-test under -race: many goroutines concurrently
	// validating + creating + deleting sessions. Ensures the
	// single mu covers every state mutation.
	s := newStore(t)
	plaintext, _ := s.MintInitial("admin")

	var wg sync.WaitGroup
	const goroutines = 32
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if err := s.Verify("admin", plaintext); err != nil {
					t.Errorf("Verify in goroutine: %v", err)
				}
				raw, err := s.CreateSession("admin")
				if err != nil {
					t.Errorf("CreateSession: %v", err)
					return
				}
				if _, err := s.ValidateSession(raw); err != nil {
					t.Errorf("ValidateSession: %v", err)
				}
				s.DeleteSession(raw)
			}
		}()
	}
	wg.Wait()
}

func TestPasswordPlaintextNeverPersisted(t *testing.T) {
	// Defensive guard against a future refactor that accidentally
	// adds a plaintext field. We stat the on-disk file after a
	// MintInitial and confirm the plaintext doesn't appear in it.
	dir := t.TempDir()
	path := filepath.Join(dir, "adminauth.json")
	s, _ := OpenStore(path)
	plaintext, _ := s.MintInitial("admin")

	raw, err := readFileOrEmpty(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, plaintext) {
		t.Fatal("plaintext password leaked into on-disk credentials file")
	}
	// Bcrypt hash should not start with the plaintext either —
	// trivial sanity that the persisted hash is actually hashed.
	if strings.HasPrefix(raw, plaintext) {
		t.Fatal("on-disk record begins with plaintext")
	}
}

func TestPersistedHashIsBcryptShape(t *testing.T) {
	s := newStore(t)
	plaintext, _ := s.MintInitial("admin")
	s.mu.Lock()
	hash := s.user.PasswordHash
	s.mu.Unlock()
	// bcrypt.CompareHashAndPassword validates structural correctness
	// AND the password match — if it parses + verifies, we know the
	// stored value is a real bcrypt hash, not plaintext / unsalted
	// SHA.
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)); err != nil {
		t.Fatalf("stored hash does not validate against plaintext: %v", err)
	}
}

// readFileOrEmpty reads the on-disk credentials file as a string;
// missing/empty returns the empty string. Used by the
// plaintext-never-persisted guard.
func readFileOrEmpty(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}
