// Package auth manages bearer tokens: generation, hashed storage, and
// request-time validation.
//
// Tokens are 256-bit random values, base64url-encoded without padding (43
// chars). The store persists only SHA-256 hashes — a stolen tokens.json
// cannot be used to construct a working token. Token IDs are the first 12
// hex chars of the hash, stable and unique enough for a household-sized
// device set.
//
// The store is concurrent-safe within a single process, and survives out-of-
// process writes (e.g. `bridge pair` running while `bridge serve` is up): on
// each Validate, Store stats the tokens file and reloads if the mtime has
// advanced. Writes use atomic rename so a reader never sees a torn file.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// rawTokenBytes is the number of random bytes per minted token.
	// 32 bytes → 256 bits → 43 base64url chars (no padding).
	rawTokenBytes = 32

	// tokenIDLen is the number of hex chars used as a human-visible token
	// ID. 12 chars = 48 bits, plenty of uniqueness for a household device
	// set while still fitting comfortably in a status display.
	tokenIDLen = 12
)

// Token is the stored, hash-only record for a paired client.
//
// LastClientVersion / LastClientVersionAt record the iOS app version
// the client most recently identified itself as via the
// X-Client-Version request header (additive in protocol v1; absent on
// older clients). Used by the updater to refuse auto-installing a
// candidate whose MinClientVersion would orphan a still-active iOS
// build that hasn't shipped through the App Store yet.
type Token struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	Hash                string    `json:"hash"` // SHA-256 hex of the raw token bytes
	CreatedAt           time.Time `json:"createdAt"`
	LastUsedAt          time.Time `json:"lastUsedAt,omitempty"`
	LastClientVersion   string    `json:"lastClientVersion,omitempty"`
	LastClientVersionAt time.Time `json:"lastClientVersionAt,omitempty"`
}

// lastUsedFlushInterval is the shortest interval between persist() calls
// driven by LastUsedAt updates. A busy /v1/manifest poll loop otherwise
// rewrites tokens.json on every request, which is gratuitous disk I/O
// proportional to request rate.
const lastUsedFlushInterval = 30 * time.Second

// Store is an in-memory view over a JSON-backed token file. Safe for
// concurrent use by readers (Validate) and writers (Mint / Revoke).
type Store struct {
	path string

	mu            sync.Mutex
	tokens        []Token
	loaded        time.Time // mtime of tokens file when we last loaded
	isEmpty       bool      // tokens file didn't exist when we last looked
	lastUsedFlush time.Time // last persist() driven by a LastUsedAt update
}

// OpenStore opens (or initializes an empty) store at path. Missing file is
// not an error — the first Mint will create it.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path}
	// reload requires s.mu per its contract; take it even though we are
	// pre-publication so the locking discipline is consistent.
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// reload refreshes the in-memory view from disk unconditionally. Caller must
// hold s.mu.
func (s *Store) reload() error {
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.tokens = nil
		s.isEmpty = true
		s.loaded = time.Time{}
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat token store: %w", err)
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read token store: %w", err)
	}
	var tokens []Token
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &tokens); err != nil {
			return fmt.Errorf("parse token store: %w", err)
		}
	}
	// Preserve in-memory LastUsedAt bumps that the 30 s debounce in
	// Validate hasn't written to disk yet. Without this, an out-of-process
	// write (e.g. a concurrent `bridge pair` appending a new token) fires
	// reloadIfStale, which overwrites our token slice with disk contents
	// whose LastUsedAt timestamps predate the in-memory bumps — wiping the
	// debounce's in-flight work. The invariant is "in-memory LastUsedAt
	// never regresses across reload"; enforce it here by keeping whichever
	// timestamp is later per token ID.
	if len(s.tokens) > 0 {
		prior := make(map[string]time.Time, len(s.tokens))
		for _, old := range s.tokens {
			prior[old.ID] = old.LastUsedAt
		}
		for i := range tokens {
			if p, ok := prior[tokens[i].ID]; ok && p.After(tokens[i].LastUsedAt) {
				tokens[i].LastUsedAt = p
			}
		}
	}
	s.tokens = tokens
	s.isEmpty = false
	s.loaded = info.ModTime()
	return nil
}

// reloadIfStale compares the current file mtime to what we loaded and reloads
// if it's newer. Called from Validate so a `bridge pair` run picks up
// automatically in a concurrently-running `bridge serve`. Caller must hold mu.
func (s *Store) reloadIfStale() error {
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		if !s.isEmpty {
			s.tokens = nil
			s.isEmpty = true
			s.loaded = time.Time{}
		}
		return nil
	}
	if err != nil {
		return err
	}
	if info.ModTime().After(s.loaded) {
		return s.reload()
	}
	return nil
}

// persist writes the current tokens to disk atomically (tmp + rename).
// Caller must hold mu.
func (s *Store) persist() error {
	// 0o700 on the parent dir matches the 0o600 file mode — keeps the
	// whole token store inaccessible on multi-user hosts.
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("mkdir token store: %w", err)
	}
	data, err := json.MarshalIndent(s.tokens, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// os.CreateTemp creates the file with mode 0o600 — no explicit Chmod
	// is needed and a redundant one would just add a syscall.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".tokens-*.json")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	tmpName = "" // suppress defer cleanup
	if info, err := os.Stat(s.path); err == nil {
		s.loaded = info.ModTime()
		s.isEmpty = false
	}
	// Every successful persist resets the LastUsedAt debounce clock —
	// whether the persist was driven by Validate, Mint, Revoke, or
	// FlushLastUsed — so callers don't have to remember to stamp it
	// themselves and Mint/Revoke also get the debounce benefit for free.
	s.lastUsedFlush = time.Now()
	return nil
}

// Mint creates a new token with the given human-readable name (e.g. "iPhone
// 15 Pro"), persists the hash, and returns both the raw token (show once,
// to the user) and the stored record. Names need not be unique.
func (s *Store) Mint(name string) (rawToken string, tok Token, err error) {
	if name == "" {
		return "", Token{}, errors.New("name must not be empty")
	}
	var buf [rawTokenBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", Token{}, fmt.Errorf("random: %w", err)
	}
	rawToken = base64.RawURLEncoding.EncodeToString(buf[:])
	hashBytes := sha256.Sum256([]byte(rawToken))
	hashHex := hex.EncodeToString(hashBytes[:])

	tok = Token{
		ID:        hashHex[:tokenIDLen],
		Name:      name,
		Hash:      hashHex,
		CreatedAt: time.Now().UTC(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return "", Token{}, err
	}
	s.tokens = append(s.tokens, tok)
	if err := s.persist(); err != nil {
		// Roll back in-memory state so a failed write doesn't leave us
		// inconsistent with disk.
		s.tokens = s.tokens[:len(s.tokens)-1]
		return "", Token{}, err
	}
	return rawToken, tok, nil
}

// Validate checks a raw token against the store. Returns the matching Token
// and true on a hit, a zero Token and false on miss. Uses constant-time hash
// comparison.
//
// On a hit Validate updates LastUsedAt in memory and persists lazily —
// at most once per lastUsedFlushInterval — so a busy request path
// doesn't rewrite tokens.json on every hit. A persist failure is logged
// and ignored because the primary work (validation) already succeeded;
// log visibility ensures silent disk issues don't go unnoticed.
func (s *Store) Validate(rawToken string) (Token, bool) {
	if rawToken == "" {
		return Token{}, false
	}
	hashBytes := sha256.Sum256([]byte(rawToken))
	hashHex := hex.EncodeToString(hashBytes[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reloadIfStale() // best-effort
	for i := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(s.tokens[i].Hash), []byte(hashHex)) == 1 {
			// The token struct wants a wall-clock UTC value so the JSON
			// round-trip is readable; the debounce gate uses `time.Since`
			// which reads the monotonic clock and so survives NTP jumps.
			s.tokens[i].LastUsedAt = time.Now().UTC()
			if time.Since(s.lastUsedFlush) >= lastUsedFlushInterval {
				if err := s.persist(); err != nil {
					log.Printf("auth: persist LastUsedAt: %v", err)
				}
				// persist() stamps `lastUsedFlush` on success; nothing to
				// do here on either branch.
			}
			return s.tokens[i], true
		}
	}
	return Token{}, false
}

// FlushLastUsed forces a persist of any in-memory LastUsedAt updates
// that the debounce in Validate has not yet written. Call on clean
// shutdown so a just-before-exit validate doesn't lose its timestamp.
// persist() itself updates `lastUsedFlush`, so nothing else to do here.
func (s *Store) FlushLastUsed() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persist()
}

// RecordClientVersion stores the iOS app version a client identified
// itself as via the X-Client-Version request header. Called from the
// authed() middleware on every authenticated request.
//
// To avoid churning tokens.json on every request, the write piggybacks
// on the existing 30-second LastUsedAt debounce: we update the
// in-memory fields unconditionally (cheap), but only persist if the
// version actually changed since last persist OR if the debounce
// window has elapsed. A version that hasn't changed is the common
// case (same app, same build, request after request) and skips disk
// I/O entirely.
//
// id is the token ID returned by Validate. version is the raw header
// value; whitespace is trimmed and over-long values are truncated to
// 64 chars (defence against a misbehaving client filling the header
// with junk and ballooning tokens.json).
//
// No-op when id or version is empty (e.g. an old iOS client that
// doesn't send the header).
func (s *Store) RecordClientVersion(id, ver string) {
	ver = strings.TrimSpace(ver)
	if id == "" || ver == "" {
		return
	}
	if len(ver) > maxClientVersionLen {
		ver = ver[:maxClientVersionLen]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.tokens {
		if s.tokens[i].ID != id {
			continue
		}
		// Common case: same version, no need to touch fields or disk.
		if s.tokens[i].LastClientVersion == ver {
			return
		}
		s.tokens[i].LastClientVersion = ver
		s.tokens[i].LastClientVersionAt = time.Now().UTC()
		// A version *change* is rare (App Store update, dev build) and
		// worth persisting promptly so the updater's compat gate sees
		// the fresh data on the next poll. Skip the LastUsedAt
		// debounce on this branch.
		if err := s.persist(); err != nil {
			log.Printf("auth: persist client-version: %v", err)
		}
		return
	}
}

// maxClientVersionLen is the upper bound on the X-Client-Version value
// we store. iOS CFBundleShortVersionString is dotted ints (e.g. "1.2.3"
// or "1.2.3-build42"), well under this cap; anything larger is junk
// from a misbehaving client.
const maxClientVersionLen = 64

// List returns a copy of the stored tokens (hashes only — raw tokens cannot
// be recovered from the store).
func (s *Store) List() []Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reloadIfStale()
	out := make([]Token, len(s.tokens))
	copy(out, s.tokens)
	return out
}

// Revoke removes the token with the given ID. Returns ErrNotFound if no such
// token exists.
func (s *Store) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return err
	}
	for i, tok := range s.tokens {
		if tok.ID == id {
			s.tokens = append(s.tokens[:i], s.tokens[i+1:]...)
			return s.persist()
		}
	}
	return ErrNotFound
}

// ErrNotFound is returned by Revoke when the given ID is unknown.
var ErrNotFound = errors.New("token not found")
