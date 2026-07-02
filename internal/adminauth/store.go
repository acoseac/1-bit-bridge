// Package adminauth manages the admin console's single-user
// authentication layer: bcrypt-hashed credentials persisted to disk,
// in-memory session tokens, and a per-(IP, username) login rate
// limiter. Gated on `deployment.mode == public` — loopback installs
// remain unauthenticated as their historical contract requires.
//
// Sessions are in-memory only; restart logs everyone out. This is
// deliberate — the pairing store has the same "in-memory state is
// fine, paired clients re-establish" property, and persisting
// sessions across restart is extra disk surface for marginal UX
// gain. The credentials file is the only on-disk state.
package adminauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// adminBcryptCost is the bcrypt work factor. 12 is a deliberate
// sweet spot (~250 ms on the slowest supported target — Windows
// arm64). Lower would weaken brute-force resistance; higher would
// make the login path feel sluggish on weak hardware. Bump only via
// a deliberate PR with target-host benchmarks.
const adminBcryptCost = 12

// rawSessionBytes is the number of random bytes per session token.
// 32 bytes → 256 bits → 43 base64url chars (no padding). Same
// sizing as auth.Store's bearer tokens.
const rawSessionBytes = 32

// Session lifetimes. Idle bump on every Validate; hard cap is the
// absolute ceiling (so an attacker who steals a session can't keep
// it alive indefinitely by polling).
const (
	SessionIdleTimeout = 24 * time.Hour
	SessionHardCap     = 7 * 24 * time.Hour
)

// Errors returned by the Store API. Callers map these to HTTP
// status codes at the handler boundary; never leak them onto the
// wire verbatim (the JSON 401 body says "unauthenticated" — the
// specific reason stays in the server-side log).
var (
	ErrInvalidCredentials = errors.New("adminauth: invalid credentials")
	ErrSessionNotFound    = errors.New("adminauth: session not found")
	ErrSessionExpired     = errors.New("adminauth: session expired")
	ErrAlreadyInitialised = errors.New("adminauth: store already has credentials; use reset-password to rotate")
	ErrNotInitialised     = errors.New("adminauth: store has no credentials (run `bridge init --public` or `bridge admin reset-password`)")
	ErrUsernameMismatch   = errors.New("adminauth: username does not match the stored admin account")
)

// userRecord is the on-disk shape. Single-user only — the file
// either contains one record or is missing. PasswordHash is the
// bcrypt output; never the plaintext.
type userRecord struct {
	Username          string    `json:"username"`
	PasswordHash      string    `json:"passwordHash"`
	CreatedAt         time.Time `json:"createdAt"`
	PasswordChangedAt time.Time `json:"passwordChangedAt"`
}

// Session is an in-memory record. The cookie value is the raw
// session ID; the map key is the SHA-256 hex digest of the same
// bytes (constant-time compare via the auth.Store / pairing.Store
// pattern). IssuedAt drives the hard cap; LastUsedAt drives the
// idle timeout.
type Session struct {
	Username   string
	IssuedAt   time.Time
	LastUsedAt time.Time
}

// Store is the concurrent-safe credentials + session manager.
// One mutex guards both — the contention is low (admin login flow
// only) and a single lock keeps the invariants obvious.
type Store struct {
	path string

	mu       sync.Mutex
	user     *userRecord
	sessions map[[sha256.Size]byte]*Session
	now      func() time.Time // injectable clock for tests
}

// OpenStore loads (or initialises as empty) the store at path. A
// missing file is not an error — IsInitialised() reports the empty
// state and the caller's startup flow decides whether to refuse
// service or proceed.
func OpenStore(path string) (*Store, error) {
	s := &Store{
		path:     path,
		sessions: make(map[[sha256.Size]byte]*Session),
		now:      time.Now,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// IsInitialised reports whether the on-disk credentials file
// exists. Used by the bridge serve startup path to refuse-to-start
// in public mode when no admin has been minted yet.
func (s *Store) IsInitialised() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.user != nil
}

// Username returns the configured admin username, or "" if not
// initialised. Used by the login form to pre-fill the field
// (single-user system, no risk in exposing the username).
func (s *Store) Username() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.user == nil {
		return ""
	}
	return s.user.Username
}

// MintInitial generates a random 16-character alphabetic password,
// bcrypts it, persists the record, and returns the plaintext for
// one-time display to the operator. Subsequent calls return
// ErrAlreadyInitialised — credentials are only ever rotated via
// ResetPassword.
//
// The plaintext lives only in the returned string and the caller's
// printed banner. Discarding the returned string is the caller's
// responsibility; no log line in this package ever sees it.
func (s *Store) MintInitial(username string) (string, error) {
	// Generate the password + bcrypt hash BEFORE taking s.mu. bcrypt at
	// cost 12 is ~250ms, and s.mu also guards Verify / ValidateSession, so
	// hashing under the lock would stall all admin auth for that window.
	// Neither call needs store state. If MintInitial then loses the
	// `s.user != nil` race (one-time startup path) the hash is wasted —
	// acceptable for a startup-only call.
	plaintext, err := generatePassword()
	if err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), adminBcryptCost)
	if err != nil {
		return "", fmt.Errorf("bcrypt: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.user != nil {
		return "", ErrAlreadyInitialised
	}
	now := s.now()
	s.user = &userRecord{
		Username:          username,
		PasswordHash:      string(hash),
		CreatedAt:         now,
		PasswordChangedAt: now,
	}
	if err := s.persist(); err != nil {
		s.user = nil
		return "", err
	}
	return plaintext, nil
}

// ResetPassword overwrites the existing credentials with a new
// bcrypt hash. Called by `bridge admin reset-password`. Active
// sessions are NOT invalidated by this — operator who wants
// sessions revoked too must restart the bridge (cheap, and the
// hard-cap is 7d so a stale session can't outlive a normal release
// cycle).
func (s *Store) ResetPassword(username, newPassword string) error {
	if newPassword == "" {
		return errors.New("adminauth: new password must not be empty")
	}
	// Hash BEFORE taking s.mu. bcrypt at cost 12 is ~250ms, and s.mu also
	// guards Verify / ValidateSession, so hashing under the lock stalls
	// all admin auth for that window. bcrypt needs only newPassword, no
	// store state. A username-mismatch caller wastes the hash on the
	// error path (rare) — acceptable for keeping the success path off the
	// lock.
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), adminBcryptCost)
	if err != nil {
		return fmt.Errorf("bcrypt: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.user != nil && s.user.Username != username {
		return ErrUsernameMismatch
	}
	now := s.now()
	// Build a FRESH userRecord pointer rather than mutating the
	// existing one in place. Two reasons (CodeRabbit Critical +
	// Major review post-PR-#292):
	//
	//  1. **Verify race**: Verify grabs `user := s.user` under
	//     the lock then releases it for the (slow) bcrypt
	//     compare. Pre-fix, ResetPassword mutating
	//     `s.user.PasswordHash` raced the bcrypt read of the
	//     same string. Pointer-swap is atomic at the language
	//     level so the captured-and-released pointer in Verify
	//     keeps observing the OLD record consistently.
	//  2. **Persist-failure rollback**: pre-fix, a persist()
	//     error returned with in-memory state already mutated
	//     to the new hash but disk still carrying the old —
	//     login would succeed against the new password until
	//     the next restart silently reverted everything.
	//     Now: snapshot prev, swap to fresh, persist; restore
	//     prev on persist error.
	prev := s.user
	next := &userRecord{
		Username:          username,
		PasswordHash:      string(hash),
		PasswordChangedAt: now,
	}
	if prev != nil {
		next.CreatedAt = prev.CreatedAt
	} else {
		next.CreatedAt = now
	}
	s.user = next
	if err := s.persist(); err != nil {
		s.user = prev // restore in-memory to match disk
		return err
	}
	return nil
}

// Verify checks the credentials and returns nil on match.
// ErrInvalidCredentials covers both wrong-user and wrong-password
// cases — never disclose which one failed to the caller (handler
// reports a single generic "invalid credentials" to the wire).
// Constant-time username compare is unnecessary (username is not
// secret); bcrypt.CompareHashAndPassword is constant-time on the
// hash side.
func (s *Store) Verify(username, password string) error {
	s.mu.Lock()
	user := s.user
	s.mu.Unlock()
	if user == nil {
		return ErrNotInitialised
	}
	if user.Username != username {
		return ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return ErrInvalidCredentials
	}
	return nil
}

// CreateSession mints a new session token for the given username.
// Returns the RAW token (43-char base64url) for the caller to set
// as a cookie value; the Store keeps only the hash. Caller MUST
// have already verified credentials via Verify.
func (s *Store) CreateSession(username string) (string, error) {
	raw, err := generateRandomToken(rawSessionBytes)
	if err != nil {
		return "", fmt.Errorf("generate session: %w", err)
	}
	digest := sha256.Sum256([]byte(raw))
	now := s.now()
	s.mu.Lock()
	s.sessions[digest] = &Session{
		Username:   username,
		IssuedAt:   now,
		LastUsedAt: now,
	}
	s.mu.Unlock()
	return raw, nil
}

// ValidateSession looks up the session by raw token, checks both
// the idle timeout and the hard cap, and bumps LastUsedAt. Returns
// a copy of the session record on success.
//
// Expired sessions are eagerly removed from the map so they don't
// accumulate. ErrSessionExpired vs ErrSessionNotFound are
// distinguished only for tests; the handler maps both to JSON 401.
func (s *Store) ValidateSession(raw string) (*Session, error) {
	if raw == "" {
		return nil, ErrSessionNotFound
	}
	digest := sha256.Sum256([]byte(raw))
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[digest]
	if !ok {
		return nil, ErrSessionNotFound
	}
	now := s.now()
	if now.Sub(sess.IssuedAt) > SessionHardCap {
		delete(s.sessions, digest)
		return nil, ErrSessionExpired
	}
	if now.Sub(sess.LastUsedAt) > SessionIdleTimeout {
		delete(s.sessions, digest)
		return nil, ErrSessionExpired
	}
	sess.LastUsedAt = now
	out := *sess
	return &out, nil
}

// DeleteSession invalidates a session by raw token. Idempotent —
// unknown tokens return nil so logout against an already-expired
// session doesn't surface an error.
func (s *Store) DeleteSession(raw string) {
	if raw == "" {
		return
	}
	digest := sha256.Sum256([]byte(raw))
	s.mu.Lock()
	delete(s.sessions, digest)
	s.mu.Unlock()
}

// SessionCount returns the live session count. Test affordance;
// production has no consumer.
func (s *Store) SessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// load reads the on-disk credentials file. Missing file leaves the
// user nil (caller checks via IsInitialised). This method locks s.mu
// internally, so callers MUST NOT hold it.
func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.user = nil
		return nil
	}
	if err != nil {
		return fmt.Errorf("read adminauth store: %w", err)
	}
	if len(raw) == 0 {
		s.user = nil
		return nil
	}
	var rec userRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("parse adminauth store: %w", err)
	}
	s.user = &rec
	return nil
}

// persist atomically rewrites the credentials file. 0o700 dir +
// 0o600 file, same hardening as auth.Store. Caller MUST hold the
// mutex.
func (s *Store) persist() error {
	if s.user == nil {
		return errors.New("adminauth: cannot persist nil user record")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("mkdir adminauth store: %w", err)
	}
	data, err := json.MarshalIndent(s.user, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".adminauth-*.json")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("chmod tmp: %w", err)
	}
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	defer func() { _ = tmp.Close() }()
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
		return fmt.Errorf("rename adminauth store: %w", err)
	}
	tmpName = "" // success — suppress the cleanup defer
	return nil
}

// passwordAlphabet excludes visually-ambiguous characters (0/O,
// 1/l/I) so an operator transcribing the printed initial password
// from a terminal banner doesn't trip on glyph collisions.
const passwordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// generatePassword returns a 16-character alphanumeric string
// drawn from passwordAlphabet using crypto/rand. 16 chars from a
// 55-character alphabet ≈ 92 bits of entropy — well above what
// bcrypt's design comfortably handles.
//
// Uses rejection sampling to eliminate modulo bias (Gemini medium
// review on PR #290). 256 % 55 = 36, so a naive `b[0] % 55` would
// make the first 36 alphabet positions ~22 % more likely than the
// last 19. We discard any byte ≥ 220 (the largest multiple of 55
// below 256) and resample. Average rejection rate ≈ 14 % — cheap.
func generatePassword() (string, error) {
	const length = 16
	alphabetLen := byte(len(passwordAlphabet))
	// Largest multiple of alphabetLen that fits in a byte. Bytes
	// in [limit, 256) are rejected and resampled.
	limit := byte(256 - (256 % int(alphabetLen)))
	out := make([]byte, length)
	for i := 0; i < length; {
		var b [1]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		if b[0] < limit {
			out[i] = passwordAlphabet[b[0]%alphabetLen]
			i++
		}
	}
	return string(out), nil
}

// generateRandomToken returns a base64url-encoded random string
// of n bytes. Used for both session tokens and any future per-
// request CSRF tokens.
func generateRandomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
