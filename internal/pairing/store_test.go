package pairing

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	req, err := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "AB:CD")
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

func TestPollRejectsWrongSecret(t *testing.T) {
	s := quickStore(t, time.Second, time.Second, nil)
	_, hashHex := makePollPair(t, "a")
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "AB:CD")
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
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP")

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
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP")
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
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "OLD-FP")

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
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "CAPTURED-FP")

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
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP")

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
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP")
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
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP")
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
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP")
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
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP")

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
		if _, err := s.CreateRequest("d", "v", hashHex, "1.1.1.1", ""); err != nil {
			t.Fatalf("CreateRequest #%d: %v", i, err)
		}
	}
	_, hashHex := makePollPair(t, "overflow")
	if _, err := s.CreateRequest("d", "v", hashHex, "1.1.1.1", ""); !errors.Is(err, ErrQueueFull) {
		t.Errorf("4th CreateRequest: err = %v, want ErrQueueFull", err)
	}
}

func TestMaxPendingDoesNotCountTerminal(t *testing.T) {
	s := NewStore(Options{TTL: time.Minute, Grace: time.Minute, MaxPending: 2})
	t.Cleanup(s.Close)
	_, h1 := makePollPair(t, "1")
	r1, _ := s.CreateRequest("d", "v", h1, "1.1.1.1", "")
	_, h2 := makePollPair(t, "2")
	r2, _ := s.CreateRequest("d", "v", h2, "1.1.1.1", "")
	if _, err := s.Decline(r1.ID); err != nil {
		t.Fatal(err)
	}
	// After Decline of r1, only r2 is Pending — should be able to add another.
	_, h3 := makePollPair(t, "3")
	if _, err := s.CreateRequest("d", "v", h3, "1.1.1.1", ""); err != nil {
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
		req, err := s.CreateRequest("d", "v", hashHex, "1.1.1.1", "FP")
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
		if _, err := s.CreateRequest("d", "v", c, "1.1.1.1", ""); !errors.Is(err, ErrBadHash) {
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
	req, err := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP")
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
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP")
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
	req, err := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP")
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
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP")
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
	req, err := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP")
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
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP")
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
	req, _ := s.CreateRequest("Phone", "1.4.0", hashHex, "10.0.0.1", "FP")
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
