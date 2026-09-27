// Package pairingcode holds the one-time codes a pairing link carries beside
// its bearer token.
//
// A `bridge://pair` link (the console's QR, or a link opened on the device)
// carried the device's long-lived bearer token, so any app that saw the URL
// saw the token too (the 2026-09-23 external audit's H1). Shipped apps
// refuse a link without `token=`, so the link keeps it, and gains `code=`: a
// random code bound to that token, redeemable once, for TTL. An app that
// understands codes redeems it over the pinned TLS connection
// (POST /v1/pairing/redeem), and the bridge rotates the token the code names
// in the same step: the device stores a token that never travelled in a
// link, and the one that did is dead once the real device has paired. An
// app that predates codes ignores the parameter and pairs with the token, as
// every app did before.
//
// Codes live in memory only. Both halves run in `bridge serve` (the console
// issues, the v1 API redeems), so nothing crosses a process, and a code lost
// to a restart costs the operator one fresh QR. Only a SHA-256 of each code
// is held, as for tokens.
package pairingcode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"sync"
	"time"
)

const (
	// TTL is how long a code stays redeemable once issued. A QR is scanned
	// while the console shows it, and ten minutes covers fetching the phone
	// from another room. It also bounds how long a link that was copied but
	// never scanned can be redeemed by whoever copied it.
	TTL = 10 * time.Minute

	// codeBytes is the random input to a code: 256 bits, encoded as 43
	// characters of unpadded base64url, the shape tokens have.
	codeBytes = 32

	// codeLength is the encoded length of a code this package issues.
	codeLength = 43

	// maxLive bounds the codes held at once. The console issues one per mint
	// or rotation, so an operator never comes near it; it bounds memory if
	// something mints in a loop, and the oldest code goes first.
	maxLive = 64
)

// Store is the bridge's set of live pairing codes. The zero value is not
// usable; construct one with New.
type Store struct {
	mu    sync.Mutex
	codes map[[sha256.Size]byte]entry
	now   func() time.Time
}

type entry struct {
	tokenID string
	issued  time.Time
}

// New returns an empty Store.
func New() *Store {
	return &Store{codes: make(map[[sha256.Size]byte]entry), now: time.Now}
}

// Issue returns a fresh code bound to tokenID. Any code already bound to that
// token is dropped: a token has at most one live code, so rotating a token in
// the console (which issues a new code) leaves the old QR's code nothing to
// redeem. Expired codes are dropped on the way.
func (s *Store) Issue(tokenID string) (string, error) {
	var buf [codeBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("pairing code: random: %w", err)
	}
	code := base64.RawURLEncoding.EncodeToString(buf[:])
	key := sha256.Sum256([]byte(code))

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, e := range s.codes {
		if e.tokenID == tokenID || now.Sub(e.issued) >= TTL {
			delete(s.codes, k)
		}
	}
	for len(s.codes) >= maxLive {
		s.evictOldestLocked()
	}
	s.codes[key] = entry{tokenID: tokenID, issued: now}
	return code, nil
}

// Take redeems code. It removes the code BEFORE judging it, so a code is
// accepted at most once whatever the answer, and returns the token ID the
// code was issued for when it was issued within TTL.
func (s *Store) Take(code string) (tokenID string, ok bool) {
	key := sha256.Sum256([]byte(code))
	s.mu.Lock()
	defer s.mu.Unlock()
	e, found := s.codes[key]
	if !found {
		return "", false
	}
	delete(s.codes, key)
	if s.now().Sub(e.issued) >= TTL {
		return "", false
	}
	return e.tokenID, true
}

// ValidShape reports whether code could be one this package issued: exactly
// 43 characters of unpadded base64url. A handler refuses anything else before
// it reaches the store.
func ValidShape(code string) bool {
	if len(code) != codeLength {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func (s *Store) evictOldestLocked() {
	var oldestKey [sha256.Size]byte
	var oldest time.Time
	first := true
	for k, e := range s.codes {
		if first || e.issued.Before(oldest) {
			oldestKey, oldest, first = k, e.issued, false
		}
	}
	if !first {
		delete(s.codes, oldestKey)
	}
}
