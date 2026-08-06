package adminauth

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// rejectionLimit must never return 0 anywhere in its supported domain
// of 1..256.
//
// The original form computed the limit as a byte:
//
//	limit := byte(256 - (256 % int(alphabetLen)))
//
// For every alphabet length that DIVIDES 256 that expression is
// byte(256) == 0, so the `b[0] < limit` acceptance test in the draw
// loop is never satisfied and generatePassword spins forever. 57
// characters kept it latent, but the passwordAlphabet docblock
// explicitly invites editing the alphabet and 64 is about the most
// natural size to land on — at which point `bridge init --public` and
// `bridge admin reset-password` hang silently on the one path that
// mints the operator's credentials.
//
// Pure function, so this table can prove termination without running
// the loop at all.
func TestRejectionLimitIsNeverZeroInSupportedDomain(t *testing.T) {
	// 1, 2, 4, 8, 16, 32, 64, 128 and 256 all divide 256 — every one
	// of them is a zero under the byte-typed form.
	for _, n := range []int{1, 2, 4, 8, 16, 32, 57, 64, 100, 128, 200, 255, 256} {
		limit := rejectionLimit(n)
		if limit <= 0 {
			t.Errorf("rejectionLimit(%d) = %d — a non-positive limit rejects every byte "+
				"and the draw loop never terminates", n, limit)
			continue
		}
		if limit > 256 {
			t.Errorf("rejectionLimit(%d) = %d, want ≤ 256 (a byte can't exceed it)", n, limit)
		}
		// Unbiased sampling needs the accepted range to be a whole
		// number of alphabet cycles...
		if limit%n != 0 {
			t.Errorf("rejectionLimit(%d) = %d is not a multiple of %d — the draw would be biased",
				n, limit, n)
		}
		// ...and the LARGEST such range, or we reject more than needed.
		if limit+n <= 256 {
			t.Errorf("rejectionLimit(%d) = %d, but %d also fits — rejecting more bytes than necessary",
				n, limit, limit+n)
		}
	}
}

// ABOVE the supported domain rejectionLimit returns 0, and that 0 is a
// CONTRACT the caller's guard depends on — not an oversight.
//
// For alphabetLen > 256 the arithmetic is right: `256 % 300` is 256, so
// the largest multiple of 300 that fits in a byte genuinely is zero. A
// single byte cannot address more than 256 positions without bias, so
// there is no correct draw to attempt. What must never happen is the
// caller entering the loop anyway — `int(b[0]) < 0` is false for every
// byte, so it would spin forever, which is the SAME failure the
// byte-overflow fix cured, reached from above instead of from a divisor.
//
// Pinning it here means a future "make rejectionLimit total by clamping"
// change has to come here and confront the guard it would silently
// disarm.
func TestRejectionLimitSignalsUnusableAboveOneByte(t *testing.T) {
	for _, n := range []int{257, 300, 512, 1000, 65536} {
		if limit := rejectionLimit(n); limit != 0 {
			t.Errorf("rejectionLimit(%d) = %d, want 0 — the caller reads 0 as "+
				"'refuse this alphabet'; a non-zero value here would admit a biased draw", n, limit)
		}
	}
	// Non-positive input is the other unusable case (it would also
	// divide by zero).
	for _, n := range []int{0, -1} {
		if limit := rejectionLimit(n); limit != 0 {
			t.Errorf("rejectionLimit(%d) = %d, want 0", n, limit)
		}
	}
}

// generateFromAlphabet must ALWAYS return — drawing for a usable
// alphabet, erroring for an unusable one, never spinning for either.
//
// Driven with an injected alphabet rather than through generatePassword,
// so the production 57-character const stays the only thing
// generatePassword ever sees. Bounded by a timeout: on a regression this
// FAILS rather than wedging the suite (the abandoned goroutine spins
// until the binary exits, which is acceptable in a run that is already
// red).
//
// The >256 cases are the second half of the same bug. `rejectionLimit`
// correctly reports 0 there ("a byte cannot address this alphabet"), and
// without the caller's guard the loop accepts no byte and hangs — the
// byte-overflow failure reached from above the range instead of from a
// divisor of it.
func TestGenerateFromAlphabetTerminatesForEverySize(t *testing.T) {
	cases := []struct {
		size    int
		wantErr bool
	}{
		{1, false}, {2, false}, {57, false}, {64, false}, {128, false}, {256, false},
		// Wider than one byte can address: must be REFUSED, not sampled.
		{257, true}, {300, true}, {512, true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d-char-alphabet", tc.size), func(t *testing.T) {
			// Bytes repeat past 256; harmless, since these cases only
			// ever assert the refusal.
			alphabet := make([]byte, tc.size)
			for i := range alphabet {
				alphabet[i] = byte(i)
			}

			type result struct {
				out string
				err error
			}
			done := make(chan result, 1)
			go func() {
				out, err := generateFromAlphabet(string(alphabet), 16)
				done <- result{out, err}
			}()

			// NewTimer + defer Stop rather than time.After, per the
			// convention the login handler's throttle sleep documents.
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			select {
			case r := <-done:
				if tc.wantErr {
					if r.err == nil {
						t.Fatalf("a %d-character alphabet was sampled instead of refused — "+
							"one byte cannot address it without bias", tc.size)
					}
					return
				}
				if r.err != nil {
					t.Fatalf("generateFromAlphabet: %v", r.err)
				}
				if len(r.out) != 16 {
					t.Fatalf("drew %d bytes, want 16", len(r.out))
				}
				// Report the POSITION, never the drawn byte. Nothing this
				// helper produces should reach a failure message — the
				// same helper backs generatePassword, and a test log is
				// exactly the wrong place for any of it to surface.
				for i := 0; i < len(r.out); i++ {
					if int(r.out[i]) >= tc.size {
						t.Fatalf("drawn byte at index %d falls outside the %d-character alphabet",
							i, tc.size)
					}
				}
			case <-timer.C:
				t.Fatalf("generateFromAlphabet never returned for a %d-character alphabet — "+
					"the rejection limit is rejecting every byte", tc.size)
			}
		})
	}
}

// generatePassword itself keeps its documented shape: 16 characters,
// all drawn from passwordAlphabet.
func TestGeneratePasswordShape(t *testing.T) {
	pw, err := generatePassword()
	if err != nil {
		t.Fatalf("generatePassword: %v", err)
	}
	if len(pw) != 16 {
		t.Errorf("password length %d, want 16", len(pw))
	}
	// Report the POSITION of an out-of-alphabet character, never the
	// character itself: this IS a real generated admin password, and a
	// test failure message is a log line like any other.
	for i, r := range pw {
		if !strings.ContainsRune(passwordAlphabet, r) {
			t.Errorf("password character at index %d is not in passwordAlphabet", i)
		}
	}
}
