package adminauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// LoginTicketTTL is how long a one-time console login ticket stays valid.
//
// Deliberately short. A ticket travels in a URL, so it can land in shell
// history, a browser's address bar and its history database; the defence is that
// it is single-use and stale within a minute, not that those places are private.
const LoginTicketTTL = 60 * time.Second

// maxLiveTickets bounds the in-memory set. Tickets are minted by an operator (or
// by a control plane acting for one), never by an anonymous request, so this is
// a sanity ceiling rather than an anti-abuse control.
const maxLiveTickets = 32

// ErrTicketInvalid covers every reason a ticket does not authenticate: unknown,
// expired, or already used. Callers must not distinguish — a redemption attempt
// should learn nothing about whether a ticket ever existed.
var ErrTicketInvalid = errors.New("login ticket is not valid")

type loginTicket struct {
	username string
	expires  time.Time
}

// MintLoginTicket issues a single-use credential that exchanges for a console
// session, so an operator (or the hosted control plane, which already holds root
// on the host) can open an authenticated console without transcribing a
// password.
//
// Tickets live in memory only: one that survived a restart would be a
// credential written to disk for no benefit, since the flow that uses it is
// seconds long.
func (s *Store) MintLoginTicket(username string) (string, error) {
	if username == "" {
		return "", errors.New("login ticket needs a username")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.user == nil || s.user.Username != username {
		// Refuse to mint for an account that does not exist, so a ticket can
		// never authenticate as someone the store has never heard of.
		return "", fmt.Errorf("no such admin user %q", username)
	}
	now := s.clock()
	for k, t := range s.tickets {
		if now.After(t.expires) {
			delete(s.tickets, k)
		}
	}
	if len(s.tickets) >= maxLiveTickets {
		return "", errors.New("too many live login tickets")
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate login ticket: %w", err)
	}
	raw := base64.RawURLEncoding.EncodeToString(buf)
	if s.tickets == nil {
		s.tickets = map[[sha256.Size]byte]loginTicket{}
	}
	s.tickets[sha256.Sum256([]byte(raw))] = loginTicket{
		username: username,
		expires:  now.Add(LoginTicketTTL),
	}
	return raw, nil
}

// RedeemLoginTicket consumes a ticket and returns the account it authenticates.
// A ticket is spent whether or not it turned out to be valid for this caller, so
// a redemption can never be retried.
func (s *Store) RedeemLoginTicket(raw string) (string, error) {
	if raw == "" {
		return "", ErrTicketInvalid
	}
	key := sha256.Sum256([]byte(raw))
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tickets[key]
	if !ok {
		return "", ErrTicketInvalid
	}
	// Delete before judging: a ticket presented once is used up either way, so a
	// caller cannot probe an expired ticket repeatedly.
	delete(s.tickets, key)
	if s.clock().After(t.expires) {
		return "", ErrTicketInvalid
	}
	return t.username, nil
}

// clock reads the injectable clock. Callers hold s.mu.
func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}
