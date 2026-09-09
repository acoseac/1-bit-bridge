package adminauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
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

// MintLoginTicket issues a single-use credential that exchanges for a console
// session, so an operator (or the hosted control plane, which already holds root
// on the host) can open an authenticated console without transcribing a
// password.
//
// Tickets are PERSISTED, in their own small file beside the store.
//
// An earlier version kept them in memory, reasoning that a ticket outliving a
// restart was a credential written to disk for no benefit. That was wrong, and
// only a live test found it: `bridge admin login-link` runs as a SEPARATE
// PROCESS from the serving bridge, so a ticket minted in the CLI's memory died
// with the CLI and the server redeemed nothing. Every unit test passed, because
// each minted and redeemed inside one process.
//
// What is stored is the SHA-256, never the ticket, in a 0600 file alongside the
// password hashes that already live there — so the disk gains no credential it
// did not already hold, and the single-use, 60-second properties still come
// from the record being deleted on presentation.
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
	live, _ := prunedTickets(s.readTicketsLocked(), now)
	if len(live) >= maxLiveTickets {
		return "", errors.New("too many live login tickets")
	}
	buf := make([]byte, 32)
	// No error check: since Go 1.24 crypto/rand.Read never returns one — it
	// crashes the program irrecoverably rather than handing back short or
	// predictable bytes. A branch here would be unreachable, and would suggest
	// to a reader that a weaker fallback exists somewhere.
	rand.Read(buf)
	raw := base64.RawURLEncoding.EncodeToString(buf)
	live[hashTicket(raw)] = persistedTicket{
		Username:  username,
		ExpiresAt: now.Add(LoginTicketTTL).UnixNano(),
	}
	if err := s.writeTicketsLocked(live); err != nil {
		return "", err
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
	key := hashTicket(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Read from disk every time: the process that minted this is usually not
	// the process redeeming it.
	now := s.clock()
	live, pruned := prunedTickets(s.readTicketsLocked(), now)
	t, ok := live[key]
	if !ok {
		// Rewrite ONLY when pruning removed something, so expired records do
		// not accumulate on a store nobody successfully logs into. The gate
		// is what the comment always claimed and the code never did: this
		// branch is the unauthenticated one, so an ungated write here is a
		// durable rename an anonymous caller can trigger at will, on the
		// mutex every authenticated console request also takes.
		if pruned {
			_ = s.writeTicketsLocked(live)
		}
		return "", ErrTicketInvalid
	}
	// Delete before judging: a ticket presented once is used up either way, so a
	// caller cannot probe one repeatedly while waiting for a clock edge.
	delete(live, key)
	if err := s.writeTicketsLocked(live); err != nil {
		return "", err
	}
	if now.After(time.Unix(0, t.ExpiresAt)) {
		return "", ErrTicketInvalid
	}
	// Re-assert the account under the lock we already hold. MintLoginTicket
	// checks this, but the two happen in different PROCESSES with up to
	// LoginTicketTTL between them, and CreateSession validates nothing — so
	// a rename inside the window would otherwise mint a fully-privileged
	// session for a username the store no longer has.
	if s.user == nil || s.user.Username != t.Username {
		return "", ErrTicketInvalid
	}
	return t.Username, nil
}

// persistedTicket is one record on disk. The map key is the hex SHA-256 of the
// ticket, so the file never contains a usable credential.
type persistedTicket struct {
	Username  string `json:"username"`
	ExpiresAt int64  `json:"expiresAt"`
}

func hashTicket(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ticketPath is the sidecar beside the store. Separate from adminauth.json so a
// ticket write cannot disturb the password record, which matters more.
func (s *Store) ticketPath() string {
	dir, base := filepath.Split(s.path)
	return filepath.Join(dir, strings.TrimSuffix(base, filepath.Ext(base))+"-tickets.json")
}

// readTicketsLocked returns what is on disk, or an empty set. A damaged or
// missing file reads as empty: the only consequence is that a link must be
// minted again, which is strictly better than failing a login path over it.
func (s *Store) readTicketsLocked() map[string]persistedTicket {
	out := map[string]persistedTicket{}
	raw, err := os.ReadFile(s.ticketPath())
	if err != nil {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]persistedTicket{}
	}
	return out
}

func (s *Store) writeTicketsLocked(tickets map[string]persistedTicket) error {
	if len(tickets) == 0 {
		// Nothing live: remove the file rather than leaving an empty one.
		if err := os.Remove(s.ticketPath()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clear login tickets: %w", err)
		}
		return nil
	}
	body, err := json.Marshal(tickets)
	if err != nil {
		return fmt.Errorf("encode login tickets: %w", err)
	}
	// A UNIQUE staging name, not "<path>.tmp". Two PROCESSES write this file —
	// `bridge admin login-link` mints and the serving bridge redeems, which is
	// the whole reason it is on disk — and Store.mu does not reach across them.
	// On one fixed name they can interleave: A truncates and writes, B
	// truncates and writes, A renames B's half-written bytes into place. The
	// file then fails to parse and every live ticket is lost. os.CreateTemp
	// gives each writer its own file, so a rename only ever commits bytes that
	// writer produced. It also creates at 0600 modulo umask, and umask can only
	// REMOVE bits, so the Chmod below is belt-and-braces against filesystems
	// that widen on close — the convention auth.Store already follows.
	//
	// The read-modify-write is still not serialised between processes, so two
	// simultaneous mints can lose one of the two tickets. That is survivable in
	// a way a corrupt file is not: the caller mints again. An interprocess lock
	// was declined for the reason `bridge restore` gives for narrowing rather
	// than locking — a stale lockfile after an unclean exit would block the
	// login path at exactly the moment an operator needs it.
	// filepath.Dir, not the dir half of filepath.Split: Split returns "" for
	// a path with no separator, and os.CreateTemp("") stages in os.TempDir()
	// — a different filesystem on a normal Linux host, where the rename then
	// fails EXDEV. Dir returns "." instead, which is what auth.Store and this
	// package's own store.go already do. Production always passes an absolute
	// path, so this is hardening, not a live fix.
	path := s.ticketPath()
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("stage login tickets: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod login tickets: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write login tickets: %w", err)
	}
	// Sync before the rename, like every sibling persist site in the tree
	// (adminauth/store.go, auth/auth.go, config/config.go).
	// RenameWithRetry fsyncs the directory ENTRY; nothing else flushes the
	// CONTENTS, so a crash could publish a durable entry to a file whose
	// blocks were never written. Fail-safe either way — zeroed bytes fail
	// json.Unmarshal and read as an empty set — but "each site keeps its own
	// Chmod / Sync / parent-dir fsync" is the rule.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync login tickets: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close login tickets: %w", err)
	}
	if err := atomicwrite.RenameWithRetry(tmpName, path); err != nil {
		return fmt.Errorf("commit login tickets: %w", err)
	}
	tmpName = "" // renamed away; the defer must not remove the committed file
	return nil
}

// prunedTickets drops every expired record and reports whether it dropped
// any.
//
// The bool is load-bearing, not a convenience: the miss branch of
// RedeemLoginTicket is reached by an UNAUTHENTICATED, unthrottled request,
// and it used to rewrite the file unconditionally while its own comment
// said "still rewrite when pruning removed something". There was no way to
// ask. Every bogus ticket probe therefore cost a CreateTemp + Write +
// Chmod + Close + rename-with-parent-fsync, under the same s.mu that
// ValidateSession takes on every authenticated console request — measured
// at 3.93 ms/req against 159 us idle, with eight flooding clients taking an
// authenticated GET /api/stats from 278 us to 33.1 ms. It was also a
// one-request oracle for "a login link is live right now", and it handed
// the documented cross-process clobber to an anonymous caller.
func prunedTickets(in map[string]persistedTicket, now time.Time) (map[string]persistedTicket, bool) {
	out := make(map[string]persistedTicket, len(in))
	for k, t := range in {
		if now.Before(time.Unix(0, t.ExpiresAt)) {
			out[k] = t
		}
	}
	return out, len(out) != len(in)
}

// clock reads the injectable clock. Callers hold s.mu.
func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}
