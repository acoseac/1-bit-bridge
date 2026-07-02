package adminauth

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestResetPasswordConcurrentWithVerify exercises the bcrypt-off-lock
// change (bridge02-03 review, finding B): ResetPassword and MintInitial
// now compute their bcrypt hash BEFORE taking s.mu, while the userRecord
// pointer swap stays under the lock. This races ResetPassword against
// concurrent Verify calls to confirm (a) no data race (run under -race)
// and (b) the final password lands and the stale one is rejected — i.e.
// moving the hash off-lock didn't reintroduce the Verify-vs-ResetPassword
// race the pointer-swap was designed to close.
func TestResetPasswordConcurrentWithVerify(t *testing.T) {
	s := newStore(t)
	if _, err := s.MintInitial("admin"); err != nil {
		t.Fatalf("MintInitial: %v", err)
	}

	pw := func(i int) string { return fmt.Sprintf("strong-pw-%02d-XYZ", i) }
	if err := s.ResetPassword("admin", pw(0)); err != nil {
		t.Fatalf("seed ResetPassword: %v", err)
	}

	// Hammer Verify concurrently while passwords rotate. The result is
	// ignored (the password is mid-rotation); the point is to race the
	// bcrypt read against ResetPassword's pointer swap under -race.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = s.Verify("admin", pw(0))
				}
			}
		}()
	}

	const rounds = 6
	for i := 1; i < rounds; i++ {
		if err := s.ResetPassword("admin", pw(i)); err != nil {
			t.Fatalf("ResetPassword round %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()

	if err := s.Verify("admin", pw(rounds-1)); err != nil {
		t.Fatalf("Verify final password: %v", err)
	}
	if err := s.Verify("admin", pw(0)); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Verify stale password: got %v, want ErrInvalidCredentials", err)
	}
}
