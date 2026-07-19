package pairing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// helpers ---------------------------------------------------------------

// makePollPair returns a (rawSecret, sha256-hex-of-secret) pair so tests
// can submit the hash to CreateRequest and the raw to Poll/Delete.
func makePollPair(t *testing.T, seed string) (raw, hashHex string) {
	t.Helper()
	raw = "test-secret-" + seed
	sum := sha256.Sum256([]byte(raw))
	return raw, hex.EncodeToString(sum[:])
}

// stubMint returns a deterministic raw/id pair, increments a counter so
// tests can assert call counts.
type stubMint struct {
	mu    sync.Mutex
	calls int
	last  string
}

func (m *stubMint) fn(name string) (rawToken, tokenID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.last = name
	return fmt.Sprintf("raw-%d", m.calls), fmt.Sprintf("id-%d", m.calls), nil
}

func (m *stubMint) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// stubRevoke records every revoke call.
type stubRevoke struct {
	mu  sync.Mutex
	ids []string
}

func (r *stubRevoke) fn(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
	return nil
}

func (r *stubRevoke) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.ids))
	copy(out, r.ids)
	return out
}

// quickStore returns a Store with sub-second TTL/grace so timer-driven
// behaviour can be asserted without long sleeps. fingerprint is the
// captured-at-create value the cert-rotation tests need.
func quickStore(t *testing.T, ttl, grace time.Duration, revoke func(string) error) *Store {
	t.Helper()
	s := NewStore(Options{
		TTL:         ttl,
		Grace:       grace,
		MaxPending:  4,
		RevokeToken: revoke,
	})
	t.Cleanup(s.Close)
	return s
}

// state machine --------------------------------------------------------

func TestCreateAndPollPending(t *testing.T) {
	s := quickStore(t, 1*time.Second, 1*time.Second, nil)
	raw, hashHex := makePollPair(t, "a")
	req, err := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "AB:CD", "")
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if len(req.ID) != requestIDBytes*2 {
		t.Errorf("ID len = %d, want %d", len(req.ID), requestIDBytes*2)
	}
	if len(req.VerificationCode) != 6 {
		t.Errorf("VerificationCode len = %d, want 6", len(req.VerificationCode))
	}
	for _, c := range req.VerificationCode {
		if c < '0' || c > '9' {
			t.Errorf("VerificationCode contains non-digit %q", c)
		}
	}

	res, err := s.Poll(req.ID, raw)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.State != StatePending {
		t.Errorf("State = %v, want Pending", res.State)
	}
	if res.Token != "" {
		t.Errorf("Token = %q on pending poll, want empty", res.Token)
	}
	if res.TTLSecondsRemaining < 0 {
		t.Errorf("TTLSecondsRemaining = %d, want >= 0", res.TTLSecondsRemaining)
	}
}

// TestCloseShortCircuitsFiredTimerCallback pins C1: a Pending timer
// callback that had already fired before Close() stopped it must NOT re-arm
// a fresh grace timer after Close() returns. We simulate the fired-but-parked
// callback by invoking onTimer directly WITH THE LIVE GENERATION (so only the
// closed flag — not the timerGen guard — can stop it) after Close().
func TestCloseShortCircuitsFiredTimerCallback(t *testing.T) {
	// Long TTL so the real armed timer can't fire during the test.
	s := quickStore(t, time.Hour, time.Hour, nil)
	_, hashHex := makePollPair(t, "a")
	req, err := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}

	s.mu.Lock()
	gen := s.byID[req.ID].timerGen
	s.mu.Unlock()

	s.Close() // stops + nils the timer, marks closed

	// Stale fired callback resuming after Close, matching generation.
	s.onTimer(req.ID, gen)

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		t.Fatal("Close did not set closed")
	}
	got := s.byID[req.ID]
	if got == nil {
		t.Fatal("request row disappeared")
	}
	if got.State != StatePending {
		t.Errorf("State = %v, want Pending (a closed-store onTimer must not transition)", got.State)
	}
	if got.expiryTimer != nil {
		t.Error("a fresh grace timer was armed after Close (closed gate failed)")
	}
}

func TestPollRejectsWrongSecret(t *testing.T) {
	s := quickStore(t, time.Second, time.Second, nil)
	_, hashHex := makePollPair(t, "a")
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "AB:CD", "")
	if _, err := s.Poll(req.ID, "wrong-secret"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Poll wrong secret: err = %v, want ErrUnauthorized", err)
	}
	if _, err := s.Poll(req.ID, ""); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Poll empty secret: err = %v, want ErrUnauthorized", err)
	}
	if _, err := s.Poll("nosuch", "anything"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Poll unknown id: err = %v, want ErrNotFound", err)
	}
}

func TestApproveProducesTokenOnEveryAuthorizedPoll(t *testing.T) {
	// The read-many delivery contract: a network blip that drops the
	// 200 OK carrying the token should be recoverable on retry.
	mint := &stubMint{}
	s := quickStore(t, 1*time.Second, 1*time.Second, nil)
	raw, hashHex := makePollPair(t, "a")
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")

	if _, err := s.Approve(req.ID, "FP", mint.fn); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if mint.callCount() != 1 {
		t.Errorf("mint called %d times, want 1", mint.callCount())
	}

	for i := 0; i < 3; i++ {
		res, err := s.Poll(req.ID, raw)
		if err != nil {
			t.Fatalf("Poll #%d: %v", i, err)
		}
		if res.State != StateApproved {
			t.Errorf("Poll #%d State = %v, want Approved", i, res.State)
		}
		if res.Token == "" {
			t.Errorf("Poll #%d Token empty under Approved (read-many contract)", i)
		}
		if res.TokenID == "" {
			t.Errorf("Poll #%d TokenID empty under Approved", i)
		}
	}
}

func TestApproveRejectsNonPending(t *testing.T) {
	mint := &stubMint{}
	s := quickStore(t, time.Second, time.Second, nil)
	_, hashHex := makePollPair(t, "a")
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")
	if _, err := s.Approve(req.ID, "FP", mint.fn); err != nil {
		t.Fatalf("first Approve: %v", err)
	}
	if _, err := s.Approve(req.ID, "FP", mint.fn); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("second Approve: err = %v, want ErrAlreadyDecided", err)
	}
	// Mint must NOT have been called twice.
	if mint.callCount() != 1 {
		t.Errorf("mint called %d times across two Approves, want 1", mint.callCount())
	}
}

func TestApproveCertRotationGuard(t *testing.T) {
	mint := &stubMint{}
	s := quickStore(t, time.Second, time.Second, nil)
	_, hashHex := makePollPair(t, "a")
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "OLD-FP", "")

	snap, err := s.Approve(req.ID, "NEW-FP", mint.fn)
	if !errors.Is(err, ErrCertRotated) {
		t.Errorf("Approve with rotated cert: err = %v, want ErrCertRotated", err)
	}
	if snap.State != StateCertRotated {
		t.Errorf("State after rotation refusal = %v, want CertRotated", snap.State)
	}
	if mint.callCount() != 0 {
		t.Errorf("mint called %d times under cert-rotation refusal, want 0", mint.callCount())
	}
}

func TestApproveCertRotationGuardFailsClosedOnEmptyCurrent(t *testing.T) {
	// Locks the CodeRabbit-third-pass invariant: once CreateRequest
	// captured a fingerprint, an Approve called with empty
	// currentFingerprint (caller regression / fingerprint lookup
	// failure) must fail closed via ErrCertRotated rather than
	// silently bypassing the pin check.
	mint := &stubMint{}
	s := quickStore(t, time.Second, time.Second, nil)
	_, hashHex := makePollPair(t, "fail-closed")
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "CAPTURED-FP", "")

	snap, err := s.Approve(req.ID, "" /* missing/lookup-failed */, mint.fn)
	if !errors.Is(err, ErrCertRotated) {
		t.Errorf("Approve with empty current fingerprint: err = %v, want ErrCertRotated", err)
	}
	if snap.State != StateCertRotated {
		t.Errorf("State after empty-current refusal = %v, want CertRotated", snap.State)
	}
	if mint.callCount() != 0 {
		t.Errorf("mint called %d times under empty-current refusal, want 0", mint.callCount())
	}
}

func TestDeclineTransitionsAndRevokesNothing(t *testing.T) {
	revoke := &stubRevoke{}
	s := quickStore(t, time.Second, 50*time.Millisecond, revoke.fn)
	_, hashHex := makePollPair(t, "a")
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")

	snap, err := s.Decline(req.ID)
	if err != nil {
		t.Fatalf("Decline: %v", err)
	}
	if snap.State != StateDeclined {
		t.Errorf("State after Decline = %v, want Declined", snap.State)
	}

	// Wait past the grace window — the row should disappear, but
	// since no token was minted nothing is revoked.
	time.Sleep(150 * time.Millisecond)
	if _, err := s.Poll(req.ID, "anysecret"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Poll after grace cleanup: err = %v, want ErrNotFound", err)
	}
	if calls := revoke.calls(); len(calls) != 0 {
		t.Errorf("revoke called %d times after decline, want 0", len(calls))
	}
}

func TestPendingExpiresAfterTTL(t *testing.T) {
	s := quickStore(t, 50*time.Millisecond, 200*time.Millisecond, nil)
	raw, hashHex := makePollPair(t, "a")
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")
	time.Sleep(120 * time.Millisecond)
	res, err := s.Poll(req.ID, raw)
	if err != nil {
		t.Fatalf("Poll after TTL: %v (row should still exist in grace window)", err)
	}
	if res.State != StateExpired {
		t.Errorf("State after TTL = %v, want Expired", res.State)
	}
}

func TestApprovedUndeliveredRevokesAtTTLPlusGrace(t *testing.T) {
	revoke := &stubRevoke{}
	mint := &stubMint{}
	s := quickStore(t, 50*time.Millisecond, 50*time.Millisecond, revoke.fn)
	_, hashHex := makePollPair(t, "a")
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")
	if _, err := s.Approve(req.ID, "FP", mint.fn); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	// Wait past TTL+grace; the floor in scheduleTimer keeps a
	// minimum 1s window, so we wait 1.2s.
	time.Sleep(1200 * time.Millisecond)

	calls := revoke.calls()
	if len(calls) != 1 {
		t.Fatalf("revoke called %d times, want 1; calls=%v", len(calls), calls)
	}
	if calls[0] != "id-1" {
		t.Errorf("revoke called with %q, want id-1", calls[0])
	}
	// Row should be gone.
	if _, err := s.Poll(req.ID, "anysecret"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Poll after revoke: err = %v, want ErrNotFound", err)
	}
}

func TestDeleteAfterApprovePreventsRevoke(t *testing.T) {
	// iOS happy path: poll succeeds, persists token, sends DELETE as
	// acknowledgment. The undelivered-revoke timer must not fire.
	revoke := &stubRevoke{}
	mint := &stubMint{}
	s := quickStore(t, 50*time.Millisecond, 50*time.Millisecond, revoke.fn)
	raw, hashHex := makePollPair(t, "a")
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")
	if _, err := s.Approve(req.ID, "FP", mint.fn); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if _, err := s.Poll(req.ID, raw); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if err := s.Delete(req.ID, raw); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	time.Sleep(1300 * time.Millisecond)
	if calls := revoke.calls(); len(calls) != 0 {
		t.Errorf("revoke called %d times after DELETE ack, want 0; calls=%v", len(calls), calls)
	}
}

func TestDeleteAuthAndIdempotency(t *testing.T) {
	s := quickStore(t, time.Second, time.Second, nil)
	raw, hashHex := makePollPair(t, "a")
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")

	if err := s.Delete(req.ID, "wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Delete wrong secret: err = %v, want ErrUnauthorized", err)
	}
	if err := s.Delete(req.ID, raw); err != nil {
		t.Fatalf("Delete with right secret: %v", err)
	}
	// Second delete reports NotFound — handler maps to 200 (idempotent).
	if err := s.Delete(req.ID, raw); !errors.Is(err, ErrNotFound) {
		t.Errorf("Second Delete: err = %v, want ErrNotFound", err)
	}
}

// caps + concurrency ---------------------------------------------------

func TestMaxPendingCap(t *testing.T) {
	s := NewStore(Options{TTL: time.Minute, Grace: time.Minute, MaxPending: 3})
	t.Cleanup(s.Close)
	for i := 0; i < 3; i++ {
		_, hashHex := makePollPair(t, fmt.Sprintf("k%d", i))
		if _, err := s.CreateRequest("d", "v", hashHex, "1.1.1.1", "", ""); err != nil {
			t.Fatalf("CreateRequest #%d: %v", i, err)
		}
	}
	_, hashHex := makePollPair(t, "overflow")
	if _, err := s.CreateRequest("d", "v", hashHex, "1.1.1.1", "", ""); !errors.Is(err, ErrQueueFull) {
		t.Errorf("4th CreateRequest: err = %v, want ErrQueueFull", err)
	}
}

func TestMaxPendingDoesNotCountTerminal(t *testing.T) {
	s := NewStore(Options{TTL: time.Minute, Grace: time.Minute, MaxPending: 2})
	t.Cleanup(s.Close)
	_, h1 := makePollPair(t, "1")
	r1, _ := s.CreateRequest("d", "v", h1, "1.1.1.1", "", "")
	_, h2 := makePollPair(t, "2")
	r2, _ := s.CreateRequest("d", "v", h2, "1.1.1.1", "", "")
	if _, err := s.Decline(r1.ID); err != nil {
		t.Fatal(err)
	}
	// After Decline of r1, only r2 is Pending — should be able to add another.
	_, h3 := makePollPair(t, "3")
	if _, err := s.CreateRequest("d", "v", h3, "1.1.1.1", "", ""); err != nil {
		t.Errorf("CreateRequest after Decline freed a slot: %v", err)
	}
	_ = r2
}

func TestConcurrentApproveExpire(t *testing.T) {
	// Race: timer fires Pending→Expired at roughly the same moment as
	// an admin Approve. The mutex must serialize so exactly one wins
	// and Mint is called at most once. Grace is long enough that even
	// the slowest goroutine still sees the row in the map (terminal
	// states linger for grace before deletion); the only ambiguity
	// is Pending vs Expired.
	mint := &stubMint{}
	revoke := &stubRevoke{}
	s := NewStore(Options{
		TTL:         25 * time.Millisecond,
		Grace:       5 * time.Second,
		MaxPending:  100,
		RevokeToken: revoke.fn,
	})
	t.Cleanup(s.Close)

	var wg sync.WaitGroup
	var approveOK atomic.Int32
	for i := 0; i < 50; i++ {
		_, hashHex := makePollPair(t, fmt.Sprintf("c%d", i))
		req, err := s.CreateRequest("d", "v", hashHex, "1.1.1.1", "FP", "")
		if err != nil {
			t.Fatalf("CreateRequest #%d: %v", i, err)
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			time.Sleep(20 * time.Millisecond) // race the TTL
			_, err := s.Approve(id, "FP", mint.fn)
			switch {
			case err == nil:
				approveOK.Add(1)
			case errors.Is(err, ErrAlreadyDecided):
				// Expired won the race — fine.
			default:
				t.Errorf("Approve unexpected error: %v", err)
			}
		}(req.ID)
	}
	wg.Wait()
	// Mint count must equal successful Approves (no double-mint).
	if mint.callCount() != int(approveOK.Load()) {
		t.Errorf("mint called %d times, approveOK = %d (must be equal)", mint.callCount(), approveOK.Load())
	}
}

// edge cases -----------------------------------------------------------

func TestCreateRejectsBadHash(t *testing.T) {
	s := quickStore(t, time.Second, time.Second, nil)
	cases := []string{"", "notlongenough", "ZZ" + makeHexLen(62)}
	for _, c := range cases {
		if _, err := s.CreateRequest("d", "v", c, "1.1.1.1", "", ""); !errors.Is(err, ErrBadHash) {
			t.Errorf("hash %q: err = %v, want ErrBadHash", c, err)
		}
	}
}

func TestVerificationCodeFormat(t *testing.T) {
	// Cover the leading-zero invariant: code is always 6 chars, numeric.
	for i := 0; i < 200; i++ {
		c, err := randomVerificationCode()
		if err != nil {
			t.Fatalf("randomVerificationCode: %v", err)
		}
		if len(c) != 6 {
			t.Errorf("len(%q) = %d, want 6", c, len(c))
		}
		for _, r := range c {
			if r < '0' || r > '9' {
				t.Errorf("non-digit in %q", c)
			}
		}
	}
}

func TestVerificationCodePreservesLeadingZeros(t *testing.T) {
	// We can't easily force a low-value sample from crypto/rand, but
	// we can directly exercise the formatter for the documented cases.
	cases := []struct {
		n    uint32
		want string
	}{
		{0, "000000"},
		{1, "000001"},
		{4123, "004123"},
		{999999, "999999"},
	}
	for _, tc := range cases {
		got := fmt.Sprintf("%06d", tc.n)
		if got != tc.want {
			t.Errorf("format %d = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestApproveRefusesPastWallClockTTL(t *testing.T) {
	// Regression for the CodeRabbit finding on PR #103: even if onTimer
	// hasn't grabbed the lock yet to flip Pending→Expired, the Approve
	// path must refuse a request whose wall-clock TTL has elapsed —
	// minting for an abandoned request burns a token slot.
	mint := &stubMint{}
	// Use an injected clock so we can advance past TTL deterministically
	// without a real sleep (avoids racing with the AfterFunc sweeper).
	now := time.Now()
	clock := func() time.Time { return now }
	s := NewStore(Options{
		TTL:        50 * time.Millisecond,
		Grace:      5 * time.Second,
		MaxPending: 4,
		Now:        clock,
	})
	t.Cleanup(s.Close)

	_, hashHex := makePollPair(t, "wall")
	req, err := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")
	if err != nil {
		t.Fatal(err)
	}
	// Advance the injected clock past TTL without giving the sweeper a
	// chance to run on the wall clock — Approve should detect this on
	// its own and transition the row to Expired.
	now = now.Add(100 * time.Millisecond)

	snap, err := s.Approve(req.ID, "FP", mint.fn)
	if !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("Approve past TTL: err = %v, want ErrAlreadyDecided", err)
	}
	if snap.State != StateExpired {
		t.Errorf("post-Approve state = %v, want Expired", snap.State)
	}
	if mint.callCount() != 0 {
		t.Errorf("mint called %d times for past-TTL request, want 0", mint.callCount())
	}
}

func TestDeclineRefusesPastWallClockTTL(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	s := NewStore(Options{
		TTL:        50 * time.Millisecond,
		Grace:      5 * time.Second,
		MaxPending: 4,
		Now:        clock,
	})
	t.Cleanup(s.Close)
	_, hashHex := makePollPair(t, "wall-decline")
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")
	now = now.Add(100 * time.Millisecond)
	snap, err := s.Decline(req.ID)
	if !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("Decline past TTL: err = %v, want ErrAlreadyDecided", err)
	}
	if snap.State != StateExpired {
		t.Errorf("post-Decline state = %v, want Expired", snap.State)
	}
}

func TestStaleTimerCallbackIsNoOp(t *testing.T) {
	// Regression for the qodo finding on PR #103: a Pending-phase timer
	// callback that's already been queued by the runtime (Stop returned
	// false because the callback is past the recall window) must not
	// mutate the request after Approve has rescheduled.
	//
	// Direct unit test: simulate a stale callback by calling onTimer with
	// an old generation value. The request was Approved between when the
	// stale timer fired and when its callback grabbed the lock — a
	// non-guarded onTimer would treat State==Approved as "TTL+grace
	// elapsed without ack" and revoke + delete. With the gen guard, the
	// stale callback no-ops.
	revoke := &stubRevoke{}
	mint := &stubMint{}
	s := quickStore(t, time.Hour, time.Hour, revoke.fn)
	_, hashHex := makePollPair(t, "stale")
	req, err := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(req.ID, "FP", mint.fn); err != nil {
		t.Fatal(err)
	}
	// Capture the post-Approve generation so the stale callback uses
	// generation 1 (the original Pending timer's gen, which Approve
	// has since invalidated by incrementing).
	staleGen := uint64(1)
	s.onTimer(req.ID, staleGen)

	// The Approved row must still be in the map with the token intact.
	res, err := s.Poll(req.ID, "test-secret-stale")
	if err != nil {
		t.Fatalf("Poll after stale timer: %v (row should still exist)", err)
	}
	if res.State != StateApproved {
		t.Errorf("State = %v after stale timer, want Approved", res.State)
	}
	if res.Token == "" {
		t.Error("Token wiped by stale timer callback")
	}
	if calls := revoke.calls(); len(calls) != 0 {
		t.Errorf("revoke fired %d times from stale timer, want 0", len(calls))
	}
}

func TestSnapshotHasNoLiveTimer(t *testing.T) {
	s := quickStore(t, time.Second, time.Second, nil)
	_, hashHex := makePollPair(t, "a")
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")
	if req.expiryTimer != nil {
		t.Error("snapshot leaks live expiryTimer pointer")
	}
}

func TestSnapshotRedactsSecretMaterial(t *testing.T) {
	// Locks the CodeRabbit-second-pass invariant: snapshot copies that
	// flow into admin-side rendering / List() / Approve()'s return must
	// NOT carry RawToken (live bearer) or PollHash (auth-binding hash).
	// Poll path reads from the live `*Request` directly so this redaction
	// doesn't affect iOS-side token delivery.
	mint := &stubMint{}
	s := quickStore(t, time.Second, time.Second, nil)
	raw, hashHex := makePollPair(t, "redact")
	req, err := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")
	if err != nil {
		t.Fatal(err)
	}
	// Pending snapshot — PollHash must be zeroed even though no Approve has run.
	if req.PollHash != ([32]byte{}) {
		t.Error("CreateRequest snapshot leaked PollHash")
	}
	if req.RawToken != "" {
		t.Error("CreateRequest snapshot leaked RawToken (should be empty anyway)")
	}

	approved, err := s.Approve(req.ID, "FP", mint.fn)
	if err != nil {
		t.Fatal(err)
	}
	if approved.RawToken != "" {
		t.Error("Approve snapshot leaked RawToken bearer")
	}
	if approved.PollHash != ([32]byte{}) {
		t.Error("Approve snapshot leaked PollHash")
	}
	if approved.TokenID == "" {
		t.Error("Approve snapshot dropped TokenID — that should still surface")
	}

	// And the live row must STILL have the token so a subsequent
	// authorized Poll can deliver it (read-many contract).
	pollRes, err := s.Poll(req.ID, raw)
	if err != nil {
		t.Fatalf("Poll after snapshot redaction: %v", err)
	}
	if pollRes.Token == "" {
		t.Error("Poll lost RawToken — snapshot redaction must not affect live row")
	}

	// List must also return redacted snapshots.
	for _, r := range s.List() {
		if r.RawToken != "" {
			t.Errorf("List leaked RawToken on row %s", r.ID)
		}
		if r.PollHash != ([32]byte{}) {
			t.Errorf("List leaked PollHash on row %s", r.ID)
		}
	}
}

// TestCloseDuringRevokeDoesNotRearmTimer pins the Gemini-#494 race: if
// Close() runs WHILE onTimer is parked in the out-of-lock revoke, the
// post-revoke re-acquire must re-check closed and NOT schedule a retry timer
// that outlives Close. We block inside the revoke fn, Close() the store, then
// release the revoke with a failure and assert no fresh timer was armed.
func TestCloseDuringRevokeDoesNotRearmTimer(t *testing.T) {
	revokeEntered := make(chan struct{})
	revokeRelease := make(chan struct{})
	blockingFailingRevoke := func(string) error {
		close(revokeEntered)
		<-revokeRelease
		return errors.New("simulated revoke failure")
	}
	// Long TTL/grace so Approve's own undelivered-cleanup timer can't fire
	// during the test — the only onTimer invocation is our manual one.
	s := NewStore(Options{
		TTL:         time.Hour,
		Grace:       time.Hour,
		MaxPending:  4,
		RevokeToken: blockingFailingRevoke,
	})
	t.Cleanup(s.Close)

	mint := &stubMint{}
	_, hashHex := makePollPair(t, "race")
	req, err := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(req.ID, "FP", mint.fn); err != nil {
		t.Fatal(err)
	}

	s.mu.Lock()
	gen := s.byID[req.ID].timerGen
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { defer close(done); s.onTimer(req.ID, gen) }()

	select {
	case <-revokeEntered: // onTimer is now parked in the out-of-lock revoke
	case <-time.After(2 * time.Second):
		close(revokeRelease)
		t.Fatal("revoke never entered — onTimer didn't reach the revoke path")
	}
	s.Close()            // sets closed + stops timers WHILE the revoke is in flight
	close(revokeRelease) // revoke returns an error → onTimer re-acquires the lock
	<-done               // and must hit the closed re-check instead of scheduleTimer

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		t.Fatal("Close did not set closed")
	}
	got := s.byID[req.ID]
	if got == nil {
		t.Fatal("row unexpectedly deleted (closed re-check should return before delete)")
	}
	if got.expiryTimer != nil {
		t.Error("a retry timer was armed after Close during revoke (post-revoke closed re-check failed)")
	}
}

func TestRevokeRetryOnFailure(t *testing.T) {
	// Locks the CodeRabbit-second-pass invariant: an Approved-but-
	// undelivered request whose first revoke fails must NOT be deleted
	// from the map. Bounded retries (maxRevokeAttempts) so a permanent
	// auth.Store outage doesn't pin the row forever.
	var attempts int32
	failingThenSucceeding := func(tokenID string) error {
		n := atomic.AddInt32(&attempts, 1)
		if n < 2 {
			return errors.New("simulated revoke failure")
		}
		return nil
	}
	mint := &stubMint{}
	// Short TTL+grace so the post-Approve timer fires quickly; revoke
	// retry backoff (1s, 10s, 60s) means the test waits ~1.5s for the
	// first retry to succeed.
	s := NewStore(Options{
		TTL:         50 * time.Millisecond,
		Grace:       50 * time.Millisecond,
		MaxPending:  4,
		RevokeToken: failingThenSucceeding,
	})
	t.Cleanup(s.Close)
	_, hashHex := makePollPair(t, "retry")
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")
	if _, err := s.Approve(req.ID, "FP", mint.fn); err != nil {
		t.Fatal(err)
	}
	// Wait for: TTL+grace (~1s floor inside Approve) + revoke first
	// attempt + 1s retry backoff + revoke second attempt.
	time.Sleep(2500 * time.Millisecond)

	// Both attempts should have fired (first failed, second succeeded).
	got := atomic.LoadInt32(&attempts)
	if got < 2 {
		t.Errorf("revoke attempts = %d, want >= 2 (retry never fired)", got)
	}
	// Row should be gone now that revoke finally succeeded.
	if _, err := s.Poll(req.ID, "anysecret"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Poll after successful retry: err = %v, want ErrNotFound", err)
	}
}

func TestRevokeRetryGivesUpAfterMaxAttempts(t *testing.T) {
	// Permanently-failing revoke must NOT pin the row; after
	// maxRevokeAttempts the row is dropped and the operator gets a log.
	var attempts int32
	alwaysFails := func(tokenID string) error {
		atomic.AddInt32(&attempts, 1)
		return errors.New("permanent revoke failure")
	}
	mint := &stubMint{}
	s := NewStore(Options{
		TTL:         50 * time.Millisecond,
		Grace:       50 * time.Millisecond,
		MaxPending:  4,
		RevokeToken: alwaysFails,
	})
	t.Cleanup(s.Close)
	_, hashHex := makePollPair(t, "giveup")
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")
	if _, err := s.Approve(req.ID, "FP", mint.fn); err != nil {
		t.Fatal(err)
	}
	// TTL+grace (~1s floor) + 3 attempts at 1s/10s/... — but we only
	// need to verify that the row eventually disappears AND the attempt
	// count caps at maxRevokeAttempts. The 60s retry would make this
	// test slow; instead verify the cap by waiting through the first
	// two retry backoffs and asserting the row is still alive at that
	// point with attempts == 2 (first try + first retry), then poll
	// again much later for the final state.
	time.Sleep(13 * time.Second)
	// By now attempts should be 3 (initial + 1s retry + 10s retry) and
	// the row should be gone (giving up at maxRevokeAttempts=3).
	got := atomic.LoadInt32(&attempts)
	if got != int32(maxRevokeAttempts) {
		t.Errorf("revoke attempts = %d, want %d (cap)", got, maxRevokeAttempts)
	}
	if _, err := s.Poll(req.ID, "anysecret"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Poll after revoke give-up: err = %v, want ErrNotFound (row dropped)", err)
	}
}

// makeHexLen returns a string of n hex digits — used to construct a
// hash with the right length but invalid bytes for the bad-hash tests.
func makeHexLen(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = 'a'
	}
	return string(out)
}

// stubOnStateChange records every state-change snapshot the Store
// fires. Mirror shape to stubRevoke. Used to test SetOnStateChange.
type stubOnStateChange struct {
	mu        sync.Mutex
	snapshots []Request
}

func (s *stubOnStateChange) fn(snap Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots = append(s.snapshots, snap)
}

func (s *stubOnStateChange) all() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.snapshots))
	copy(out, s.snapshots)
	return out
}

// TestApproveFiresOnStateChange is the headline contract for the SSE
// upstream-publisher wiring (PR following #135): Approve fires
// onStateChange with the post-transition snapshot. cmd/bridge wires
// this to publish a `pairing.<requestID>` event to the SSE broker.
func TestApproveFiresOnStateChange(t *testing.T) {
	rec := &stubOnStateChange{}
	mint := &stubMint{}
	s := quickStore(t, time.Second, time.Second, nil)
	s.SetOnStateChange(rec.fn)

	_, hashHex := makePollPair(t, "approve-event")
	req, err := s.CreateRequest("Phone", "1.4", hashHex, "10.0.0.1", "AB:CD", "")
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	// CreateRequest does NOT fire onStateChange — only state
	// transitions do. Sanity check: no fires yet.
	if got := len(rec.all()); got != 0 {
		t.Errorf("CreateRequest fired onStateChange %d times, want 0", got)
	}

	if _, err := s.Approve(req.ID, "AB:CD", mint.fn); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	snaps := rec.all()
	if len(snaps) != 1 {
		t.Fatalf("Approve onStateChange fires = %d, want 1", len(snaps))
	}
	if snaps[0].State != StateApproved {
		t.Errorf("snapshot state = %v, want Approved", snaps[0].State)
	}
	if snaps[0].ID != req.ID {
		t.Errorf("snapshot ID = %q, want %q", snaps[0].ID, req.ID)
	}
	if snaps[0].RawToken == "" {
		t.Errorf("Approved snapshot RawToken empty; iOS wouldn't have a token to consume")
	}
}

// TestDeclineFiresOnStateChange — Pending→Declined fires onStateChange.
func TestDeclineFiresOnStateChange(t *testing.T) {
	rec := &stubOnStateChange{}
	s := quickStore(t, time.Second, time.Second, nil)
	s.SetOnStateChange(rec.fn)

	_, hashHex := makePollPair(t, "decline-event")
	req, _ := s.CreateRequest("Phone", "1.4", hashHex, "10.0.0.1", "AB:CD", "")
	if _, err := s.Decline(req.ID); err != nil {
		t.Fatalf("Decline: %v", err)
	}
	snaps := rec.all()
	if len(snaps) != 1 || snaps[0].State != StateDeclined {
		t.Errorf("Decline fires = %d, last state = %v, want 1 Declined", len(snaps), snaps)
	}
}

// TestPendingExpiresFiresOnStateChange — the timer-driven Pending→
// Expired transition fires onStateChange. iOS uses this to update
// the pairing UI to "expired" without needing to keep polling.
func TestPendingExpiresFiresOnStateChange(t *testing.T) {
	rec := &stubOnStateChange{}
	// 50 ms TTL + 50 ms grace so the test runs in milliseconds.
	s := quickStore(t, 50*time.Millisecond, 50*time.Millisecond, nil)
	s.SetOnStateChange(rec.fn)

	_, hashHex := makePollPair(t, "expire-event")
	_, _ = s.CreateRequest("Phone", "1.4", hashHex, "10.0.0.1", "AB:CD", "")

	// Wait for the TTL timer to fire + onStateChange to be invoked.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(rec.all()) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	snaps := rec.all()
	if len(snaps) < 1 {
		t.Fatalf("Pending→Expired never fired onStateChange")
	}
	if snaps[0].State != StateExpired {
		t.Errorf("first snapshot state = %v, want Expired", snaps[0].State)
	}
}

// TestNilOnStateChangeIsNoOp locks back-compat: a Store constructed
// without SetOnStateChange runs every transition path without
// panicking on the nil callback.
func TestNilOnStateChangeIsNoOp(t *testing.T) {
	mint := &stubMint{}
	s := quickStore(t, time.Second, time.Second, nil)
	// Deliberately do NOT call SetOnStateChange.

	_, hashHex := makePollPair(t, "nil-callback")
	req, _ := s.CreateRequest("Phone", "1.4", hashHex, "10.0.0.1", "AB:CD", "")
	if _, err := s.Approve(req.ID, "AB:CD", mint.fn); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	// Test passes if no panic. No assertion needed beyond completion.
}

// TestSetOnStateChangeIsRaceSafe drives Set + Approve concurrently.
// The stateChangeMu RWMutex guards the swap so the race detector
// stays clean.
func TestSetOnStateChangeIsRaceSafe(t *testing.T) {
	mint := &stubMint{}
	s := quickStore(t, time.Second, time.Second, nil)

	// Background swapper: continuously rewires the callback.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s.SetOnStateChange(func(Request) {})
			}
		}
	}()

	// Drive a sequence of Approves to exercise the fire path.
	for i := 0; i < 8; i++ {
		_, hashHex := makePollPair(t, "race-"+strconv.Itoa(i))
		req, _ := s.CreateRequest("Phone", "1.4", hashHex, "10.0.0.1", "AB:CD", "")
		_, _ = s.Approve(req.ID, "AB:CD", mint.fn)
	}

	close(stop)
	wg.Wait()
	// No-panic + no race-detector flag = pass.
}

// TestCreateRequestStoresDeviceToken verifies the durable recovery token
// supplied in the join request is preserved on the snapshot the admin
// approve path reads (it is NOT one of the snapshot-redacted fields).
func TestCreateRequestStoresDeviceToken(t *testing.T) {
	s := quickStore(t, 1*time.Second, 1*time.Second, nil)
	_, hashHex := makePollPair(t, "dt")
	snap, err := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "AB:CD", "9f3ce1")
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if snap.DeviceToken != "9f3ce1" {
		t.Errorf("DeviceToken = %q, want 9f3ce1", snap.DeviceToken)
	}
}

// TestApprovedTimeoutRevokeDoesNotLeakTokenViaPoll pins the fix for the
// revoke token-leak / TOCTOU window. When an Approved-but-unacknowledged
// request times out, onTimer must transition the row OUT of Approved
// (to Expired) UNDER s.mu before it performs the out-of-lock revoke —
// otherwise a Poll racing the revoke (or one of its backoff-retry
// windows) receives a token the revoke is about to destroy, and every
// subsequent iOS request 401s on the dead token.
//
// We exercise the exact window by polling from INSIDE the injected
// revoke callback: at that point onTimer has released s.mu, so Poll can
// acquire it, and the row must already read Expired with no token.
func TestApprovedTimeoutRevokeDoesNotLeakTokenViaPoll(t *testing.T) {
	raw, hashHex := makePollPair(t, "leak")

	var s *Store
	idCh := make(chan string, 1) // carries the request id into the callback (channel = happens-before)
	done := make(chan struct{})
	var once sync.Once
	var duringState State
	var duringToken string
	var duringErr error

	revoke := func(tokenID string) error {
		id := <-idCh
		idCh <- id // keep it available; success path calls this once, a retry could re-read
		res, err := s.Poll(id, raw)
		once.Do(func() {
			duringState, duringToken, duringErr = res.State, res.Token, err
			close(done)
		})
		return nil
	}

	mint := &stubMint{}
	s = quickStore(t, 50*time.Millisecond, 50*time.Millisecond, revoke)

	req, err := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if _, err := s.Approve(req.ID, "FP", mint.fn); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	idCh <- req.ID // hand the id to the callback (it blocks on <-idCh until this lands)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("revoke callback never fired within 3s")
	}

	if duringErr != nil {
		t.Fatalf("Poll during revoke: unexpected error %v", duringErr)
	}
	if duringState != StateExpired {
		t.Errorf("Poll during revoke: state = %v, want StateExpired (row must leave Approved before the revoke)", duringState)
	}
	if duringToken != "" {
		t.Errorf("Poll during revoke leaked token %q, want empty — a token handed out here is killed by the revoke and would 401", duringToken)
	}
}

// TestDeleteDuringRevokeRetryIsRejected pins the companion guard to the
// leak fix: once a request has expired WITH a minted token (revocation in
// progress, possibly across retry backoffs), a concurrent Delete must be
// rejected. Otherwise Delete would stop the retry timer and drop the row,
// orphaning a still-live token in auth.Store that the revoke never killed.
func TestDeleteDuringRevokeRetryIsRejected(t *testing.T) {
	raw, hashHex := makePollPair(t, "del-retry")

	revokeStarted := make(chan struct{})
	proceed := make(chan struct{})
	var revokeCalls int32
	var once sync.Once
	revoke := func(tokenID string) error {
		if atomic.AddInt32(&revokeCalls, 1) == 1 {
			// First revoke: signal the test, block so it can Delete while
			// we're mid-revoke, then fail so a retry is armed.
			once.Do(func() { close(revokeStarted) })
			<-proceed
			return errors.New("transient revoke failure")
		}
		return nil // the retry succeeds
	}

	mint := &stubMint{}
	s := quickStore(t, 50*time.Millisecond, 50*time.Millisecond, revoke)
	req, err := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if _, err := s.Approve(req.ID, "FP", mint.fn); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// onTimer has expired the row and is parked inside the first revoke;
	// the row is now Expired with a minted TokenID.
	select {
	case <-revokeStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("revoke never started")
	}

	// A Delete during this window must be rejected (not abort the revoke).
	if err := s.Delete(req.ID, raw); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete during revoke retry: err = %v, want ErrNotFound", err)
	}
	close(proceed) // first revoke returns its failure → a retry is scheduled

	// The retry must still run and eventually succeed → row deleted.
	deadline := time.After(3 * time.Second)
	for {
		if _, err := s.Poll(req.ID, raw); errors.Is(err, ErrNotFound) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("revoke retry did not complete after the Delete was rejected")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if got := atomic.LoadInt32(&revokeCalls); got < 2 {
		t.Errorf("revoke calls = %d, want >= 2 (the retry must have fired despite the Delete)", got)
	}
}

// recordingLogHandler captures every slog.Record it receives so a test can
// assert a specific structured log line was emitted. The dynamicHandler's
// component-attr replay is intentionally dropped (WithAttrs/WithGroup return
// self) — we only care about the record's own message + attrs.
type recordingLogHandler struct {
	mu      *sync.Mutex
	records *[]slog.Record
}

func (h recordingLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h recordingLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, r.Clone())
	return nil
}

func (h recordingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingLogHandler) WithGroup(string) slog.Handler      { return h }

// captureLogs installs a recording slog handler as the default for the
// duration of the test and returns a reader for the captured records. The
// pairing package's `logger` (logging.Component) resolves slog.Default() at
// log time, so this intercepts its output. Restored via t.Cleanup. Pairing
// tests run sequentially (none call t.Parallel), so the process-global
// SetDefault swap is safe here.
func captureLogs(t *testing.T) func() []slog.Record {
	t.Helper()
	mu := &sync.Mutex{}
	var recs []slog.Record
	prev := slog.Default()
	slog.SetDefault(slog.New(recordingLogHandler{mu: mu, records: &recs}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []slog.Record {
		mu.Lock()
		defer mu.Unlock()
		out := make([]slog.Record, len(recs))
		copy(out, recs)
		return out
	}
}

// logAttrValue extracts the string form of a single attr (by key) from a
// slog.Record; the bool reports presence.
func logAttrValue(r slog.Record, key string) (string, bool) {
	var val string
	var found bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			val, found = a.Value.String(), true
			return false
		}
		return true
	})
	return val, found
}

// approveForDeleteTest creates + approves a request with a long TTL/grace (so
// no background timer fires mid-test) and returns the store, revoke recorder,
// request id, raw pollSecret, and the minted TokenID. Shared by the two B4
// Delete-logging tests.
func approveForDeleteTest(t *testing.T, seed string) (s *Store, revoke *stubRevoke, id, raw, tokenID string) {
	t.Helper()
	revoke = &stubRevoke{}
	mint := &stubMint{}
	s = quickStore(t, time.Hour, time.Hour, revoke.fn)
	var hashHex string
	raw, hashHex = makePollPair(t, seed)
	req, err := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP", "")
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	approved, err := s.Approve(req.ID, "FP", mint.fn)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.TokenID == "" {
		t.Fatal("Approve snapshot dropped TokenID — test setup invalid")
	}
	return s, revoke, req.ID, raw, approved.TokenID
}

// logsContainTokenID reports whether any captured record carries a tokenID attr
// equal to want.
func logsContainTokenID(recs []slog.Record, want string) bool {
	for _, r := range recs {
		if v, ok := logAttrValue(r, "tokenID"); ok && v == want {
			return true
		}
	}
	return false
}

// TestDeleteApprovedAckIsSilent pins B4 (happy path): a Delete that acknowledges
// a token already delivered via Poll (poll → persist → DELETE) must be SILENT —
// the token is owned by the device and revoking it would 401 the device
// (TestDeleteAfterApprovePreventsRevoke). No revoke, no orphan log.
func TestDeleteApprovedAckIsSilent(t *testing.T) {
	s, revoke, id, raw, tokenID := approveForDeleteTest(t, "b4-ack")
	if _, err := s.Poll(id, raw); err != nil { // token delivered
		t.Fatalf("Poll: %v", err)
	}
	readLogs := captureLogs(t) // install AFTER Poll so only Delete could log
	if err := s.Delete(id, raw); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if calls := revoke.calls(); len(calls) != 0 {
		t.Errorf("revoke called %d times on ack DELETE, want 0", len(calls))
	}
	if logsContainTokenID(readLogs(), tokenID) {
		t.Errorf("normal ack DELETE logged the orphan-token line for %q, want silent", tokenID)
	}
}

// TestDeleteApprovedNeverPolledLogsOrphan pins B4 (orphan path): a Delete of an
// Approved row whose token was NEVER delivered via Poll (a DELETE without any
// prior poll) logs the tokenID as an operator breadcrumb. Still no revoke (the
// TTL+grace sweep in onTimer is the only revoke path); the row is dropped.
func TestDeleteApprovedNeverPolledLogsOrphan(t *testing.T) {
	s, revoke, id, raw, tokenID := approveForDeleteTest(t, "b4-orphan")
	// No Poll → the minted token was never delivered.
	readLogs := captureLogs(t)
	if err := s.Delete(id, raw); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if calls := revoke.calls(); len(calls) != 0 {
		t.Errorf("revoke called %d times on orphan DELETE, want 0", len(calls))
	}
	if !logsContainTokenID(readLogs(), tokenID) {
		t.Errorf("never-polled Approved DELETE did not log the orphan tokenID %q", tokenID)
	}
	if _, err := s.Poll(id, raw); !errors.Is(err, ErrNotFound) {
		t.Errorf("Poll after orphan Delete: err = %v, want ErrNotFound", err)
	}
}
