package pairingcode

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeClock is a Store clock a test moves by hand.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTestStore() (*Store, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)}
	s := New()
	s.now = c.now
	return s, c
}

func mustIssue(t *testing.T, s *Store, tokenID string) string {
	t.Helper()
	code, err := s.Issue(tokenID)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// TestAnIssuedCodeHasTheTokenShape pins the format a client can check: 43
// characters of unpadded base64url, the shape tokens have, fresh each time.
func TestAnIssuedCodeHasTheTokenShape(t *testing.T) {
	s, _ := newTestStore()
	a, b := mustIssue(t, s, "tok-a"), mustIssue(t, s, "tok-b")
	for _, code := range []string{a, b} {
		if !ValidShape(code) {
			t.Fatalf("issued code %q does not have the shape ValidShape accepts", code)
		}
	}
	if a == b {
		t.Fatal("two issues returned the same code")
	}
}

// TestACodeIsRedeemedOnce pins single use: the first Take answers the token
// the code was issued for, and every later one answers nothing.
func TestACodeIsRedeemedOnce(t *testing.T) {
	s, _ := newTestStore()
	code := mustIssue(t, s, "tok-a")
	if id, ok := s.Take(code); !ok || id != "tok-a" {
		t.Fatalf("first Take = (%q, %v), want (tok-a, true)", id, ok)
	}
	if id, ok := s.Take(code); ok {
		t.Fatalf("second Take = (%q, true), want nothing", id)
	}
	if _, ok := s.Take("never-issued-never-issued-never-issued-abcd"); ok {
		t.Fatal("a code nobody issued was taken")
	}
}

// TestACodeExpiresAtTheTTL pins the window: a code is good until TTL has
// passed since it was issued, and an expired code is removed by the Take
// that finds it, so it cannot be tried again either.
func TestACodeExpiresAtTheTTL(t *testing.T) {
	s, clock := newTestStore()
	fresh := mustIssue(t, s, "tok-fresh")
	stale := mustIssue(t, s, "tok-stale")

	clock.t = clock.t.Add(TTL - time.Nanosecond)
	if _, ok := s.Take(fresh); !ok {
		t.Fatal("a code one nanosecond short of the TTL was refused")
	}
	clock.t = clock.t.Add(time.Nanosecond)
	if _, ok := s.Take(stale); ok {
		t.Fatal("a code exactly TTL old was accepted")
	}
	clock.t = clock.t.Add(-time.Hour)
	if _, ok := s.Take(stale); ok {
		t.Fatal("an expired code was accepted once the clock went back: the refusing Take must remove it")
	}
}

// TestATokenHasOneLiveCode pins that issuing a code for a token drops the
// code it already had, so rotating a token in the console leaves the old
// QR's code nothing to redeem, while other tokens' codes are untouched.
func TestATokenHasOneLiveCode(t *testing.T) {
	s, _ := newTestStore()
	first := mustIssue(t, s, "tok-a")
	other := mustIssue(t, s, "tok-b")
	second := mustIssue(t, s, "tok-a")
	if _, ok := s.Take(first); ok {
		t.Fatal("the token's earlier code still redeemed after a new one was issued")
	}
	if id, ok := s.Take(second); !ok || id != "tok-a" {
		t.Fatalf("the token's new code = (%q, %v), want (tok-a, true)", id, ok)
	}
	if id, ok := s.Take(other); !ok || id != "tok-b" {
		t.Fatalf("another token's code = (%q, %v), want (tok-b, true)", id, ok)
	}
}

// failingReader is a random source whose every read fails.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

// TestAFailedIssueStillEndsTheTokensPreviousCode pins the order inside Issue:
// the token's previous code is dropped before the new one is drawn. The
// console issues after a rotation has already replaced the token, so an
// Issue that failed and left the old QR's code live would let that code
// rotate the token again for whoever holds the old QR.
func TestAFailedIssueStillEndsTheTokensPreviousCode(t *testing.T) {
	s, _ := newTestStore()
	old := mustIssue(t, s, "tok-a")
	other := mustIssue(t, s, "tok-b")

	s.random = failingReader{}
	if code, err := s.Issue("tok-a"); err == nil {
		t.Fatalf("Issue with a failing random source returned %q, want an error", code)
	}
	if _, ok := s.Take(old); ok {
		t.Fatal("a failed Issue left the token's previous code redeemable")
	}
	if id, ok := s.Take(other); !ok || id != "tok-b" {
		t.Fatalf("a failed Issue for tok-a touched tok-b's code: (%q, %v)", id, ok)
	}
}

// TestTheStoreHoldsAtMostMaxLiveCodes pins the memory bound: past maxLive
// the oldest codes go first, and the newest stay redeemable.
func TestTheStoreHoldsAtMostMaxLiveCodes(t *testing.T) {
	s, clock := newTestStore()
	const extra = 5
	codes := make([]string, 0, maxLive+extra)
	for i := 0; i < maxLive+extra; i++ {
		clock.t = clock.t.Add(time.Second)
		codes = append(codes, mustIssue(t, s, fmt.Sprintf("tok-%d", i)))
	}
	if n := len(s.codes); n != maxLive {
		t.Fatalf("store holds %d codes, want %d", n, maxLive)
	}
	for i, code := range codes {
		_, ok := s.Take(code)
		if want := i >= extra; ok != want {
			t.Fatalf("code %d taken = %v, want %v (the %d oldest are evicted)", i, ok, want, extra)
		}
	}
}

// TestValidShape pins what a handler refuses before the store sees it.
func TestValidShape(t *testing.T) {
	good := strings.Repeat("A", codeLength)
	for _, tc := range []struct {
		code string
		want bool
	}{
		{good, true},
		{"abcXYZ019-_" + strings.Repeat("q", codeLength-11), true},
		{"", false},
		{good[:codeLength-1], false},
		{good + "A", false},
		{"+" + good[1:], false},
		{"/" + good[1:], false},
		{good[:codeLength-1] + "=", false},
		{" " + good[1:], false},
		{"é" + good[2:], false},
	} {
		if got := ValidShape(tc.code); got != tc.want {
			t.Errorf("ValidShape(%q) = %v, want %v", tc.code, got, tc.want)
		}
	}
}
