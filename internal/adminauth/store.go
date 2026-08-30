// Package adminauth manages the admin console's single-user
// authentication layer: bcrypt-hashed credentials persisted to disk,
// in-memory session tokens, and a per-(IP, username) login rate
// limiter. Gated on `deployment.mode == public` — loopback installs
// remain unauthenticated as their historical contract requires.
//
// Sessions persist alongside the credentials, so a restart no longer
// signs every operator out.
//
// That reverses the original decision recorded here, and the reason is
// deployment shape rather than taste. On a single box a restart is a
// deliberate act by the person who is about to log back in, so
// in-memory was the right trade. On a hosted bridge the process
// restarts for reasons the operator did not ask for and may not see —
// an auto-install, a settings change that needs a bounce, a container
// reschedule — and "you are signed out again" stops reading as a
// consequence of anything. It also blocks running two replicas behind
// a load balancer, since neither can see the other's sessions.
//
// Two shapes were considered and rejected. SQLite (the tenant DB)
// would put session writes behind the same global writer mutex the
// scanner and enricher contend for, to store data that has nothing to
// do with the library. Stateless signed cookies avoid disk entirely
// but buy a key: where it lives, how it rotates, and what happens to
// every live session when it does — and a key stored in this same
// directory gains nothing over storing the sessions themselves.
// Writing them into the file that already holds the credential needs
// no new primitive, no new failure mode, and keeps instant revocation
// (deleting a row is the whole mechanism).
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
	"sync"
	"time"
	"unicode/utf8"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"golang.org/x/crypto/bcrypt"
)

// adminBcryptCost is the bcrypt work factor. 12 is a deliberate
// sweet spot (~250 ms on the slowest supported target — Windows
// arm64). Lower would weaken brute-force resistance; higher would
// make the login path feel sluggish on weak hardware. Bump only via
// a deliberate PR with target-host benchmarks.
var logger = logging.Component("adminauth")

const adminBcryptCost = 12

// minPasswordLen is the floor for an ENVIRONMENT-SEEDED credential.
//
// Deliberately not applied to ResetPassword, which has always accepted
// any non-empty string: raising the floor there would break an
// operator's existing script for a password they chose knowingly at an
// interactive prompt. The seed path is different in kind — it is
// unattended, the value arrives from automation, and nobody reads a
// warning about it. A weak secret installed by a config typo, on a
// public bridge, with nobody watching, is worth refusing outright.
const minPasswordLen = 12

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

// maxSessions caps the number of live admin sessions the store holds
// at once. A single-user console legitimately holds only a handful
// (one per browser / device); the cap bounds the map's footprint so
// a login stream that's never re-validated can't grow it without
// bound. Sessions are pruned lazily (in ValidateSession / on logout)
// AND there's no background janitor — a session created and never
// touched again would otherwise linger until its 7-day hard cap with
// no reader to reap it. CreateSession therefore sweeps expired
// sessions and, if still at the cap, evicts the least-recently-used
// one. Mirrors the RateLimiter's maxBuckets guard. 1024 is generous
// headroom for any realistic single-operator deployment; the LRU
// eviction never targets the active session.
const maxSessions = 1024

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

// storeFile is the on-disk envelope: the credential plus the live
// sessions. Sessions are keyed by the HEX of the token digest — the
// same value the in-memory map keys on, rendered as a string because
// JSON object keys must be strings. The raw token is never written;
// only its SHA-256, exactly as in memory.
type storeFile struct {
	User     *userRecord         `json:"user"`
	Sessions map[string]*Session `json:"sessions,omitempty"`
}

// sessionFlushInterval debounces LastUsedAt writes. Every
// authenticated request bumps the timestamp, and persisting each one
// would put an fsync on the hot path of the whole console. Bounded to
// one write per interval, with FlushSessions landing the remainder at
// shutdown — the auth.Store lastUsedFlush idiom, same reasoning and
// the same 30 s window.
//
// What a crash inside the window costs: up to 30 s of idle-timeout
// credit, so a session could expire marginally earlier than it should.
// Nothing is lost that a re-login does not restore.
const sessionFlushInterval = 30 * time.Second

// Store is the concurrent-safe credentials + session manager.
// One mutex guards both — the contention is low (admin login flow
// only) and a single lock keeps the invariants obvious.
type Store struct {
	path string

	mu       sync.Mutex
	user     *userRecord
	sessions map[[sha256.Size]byte]*Session
	now      func() time.Time // injectable clock for tests

	// sessionsDirty marks LastUsedAt bumps not yet on disk;
	// lastSessionFlush is when the last write landed. Both guarded by
	// mu.
	sessionsDirty    bool
	lastSessionFlush time.Time
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
	// Opportunistic cleanup: drop any sessions past their idle timeout
	// or hard cap. Nothing sweeps sessions in the background (see the
	// maxSessions doc), so doing it on each new login keeps the map
	// proportional to live sessions rather than to lifetime logins.
	// Cheap — bounded by maxSessions, so the O(N) scan is trivial.
	s.sweepExpiredSessionsLocked(now)
	// Hard cap: if the map is still at the ceiling after the sweep (all
	// sessions genuinely live), evict the least-recently-used one so a
	// login stream can't grow it without bound.
	// Evict in a LOOP, not a single conditional: if the map ever exceeds the
	// ceiling (a lowered cap, or a future path that inserts in bulk) one
	// eviction wouldn't bring it back under and the bound would silently stop
	// holding (Gemini, post-merge review of #531). The no-progress guard makes
	// the loop terminate even if a future evict implementation can no-op, so it
	// can never spin while holding s.mu.
	for len(s.sessions) >= maxSessions {
		before := len(s.sessions)
		s.evictOldestSessionLocked()
		if len(s.sessions) >= before {
			break
		}
	}
	s.sessions[digest] = &Session{
		Username:   username,
		IssuedAt:   now,
		LastUsedAt: now,
	}
	// A login is rare, so this one writes synchronously rather than
	// riding the debounce: the whole point is that the session survives
	// a restart, and a login that is not durable until the next
	// validate has a window where it is not.
	//
	// A write failure does NOT fail the login. The session is live in
	// this process and works; it simply will not survive a restart,
	// which is the behaviour every session had before persistence
	// existed. Refusing a valid login because the disk hiccuped would
	// be the worse trade.
	if err := s.persistSessionsLocked(now); err != nil {
		logger.Error("persist session on login", "err", err)
	}
	s.mu.Unlock()
	return raw, nil
}

// SeedFromEnv initialises the credential from the environment when the
// store is empty, so a bridge can be provisioned without anyone typing
// a password into it.
//
// This is the step that does not scale otherwise. `bridge admin
// reset-password` is interactive and prints a generated secret to a
// terminal; on a host nobody has a shell on, that is not a step anyone
// can take. Reading the credential from the platform's own secret
// mechanism is how every container-native service does this.
//
//	BRIDGE_ADMIN_USERNAME       — optional, defaults to "admin"
//	BRIDGE_ADMIN_PASSWORD       — the secret
//	BRIDGE_ADMIN_PASSWORD_FILE  — a path to read it from; wins over the
//	                              inline form, because a mounted secret
//	                              file does not appear in `ps`, `docker
//	                              inspect`, or a crash dump of the
//	                              environment the way a variable does
//
// Three properties worth stating, because each is a decision:
//
// It seeds ONLY an empty store. A configured bridge whose environment
// still carries the variable must not have its password reset on every
// restart — that would make the env the credential rather than the
// seed, and a rotated password would be silently undone by a bounce.
// Rotation stays `bridge admin reset-password`.
//
// It does NOT force a change at first login. That was in the plan and
// is wrong for the case this exists for: the secret is issued by the
// platform, there is no human at first login to change it, and a forced
// change would break the automation the seeding is for. A human-issued
// password is the operator's to manage.
//
// A password below minPasswordLen is REFUSED rather than trimmed or
// padded. Seeding is a security-establishing act; quietly accepting a
// weak secret because it arrived by automation is the wrong direction
// to fail.
//
// Returns (seeded, error). seeded=false with a nil error means "no
// credential in the environment", which is not an error — an
// interactively-provisioned bridge is the normal case.
//
// The caller announces the seeding. This package logs nothing about it
// on purpose: `bridge serve` already writes operator-facing lines to
// stderr, and "your admin password was just set from the environment"
// is something the person watching a first boot needs to see there, not
// something to go looking for in a log.
func (s *Store) SeedFromEnv() (bool, error) {
	if s.IsInitialised() {
		return false, nil
	}
	password, err := passwordFromEnv()
	if err != nil {
		return false, err
	}
	if password == "" {
		return false, nil
	}
	username := strings.TrimSpace(os.Getenv("BRIDGE_ADMIN_USERNAME"))
	if username == "" {
		username = "admin"
	}
	if err := s.SetInitialPassword(username, password); err != nil {
		return false, err
	}
	return true, nil
}

// passwordFromEnv reads the seed, preferring the file form.
func passwordFromEnv() (string, error) {
	if path := strings.TrimSpace(os.Getenv("BRIDGE_ADMIN_PASSWORD_FILE")); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			// Loud: the operator asked for a file and it was unreadable.
			// Falling back to the inline variable here would silently use
			// a different credential than the one they configured.
			return "", fmt.Errorf("read BRIDGE_ADMIN_PASSWORD_FILE %q: %w", path, err)
		}
		// A mounted secret file conventionally ends with a newline, and
		// the trailing byte is not part of the password.
		return strings.TrimSpace(string(raw)), nil
	}
	return strings.TrimSpace(os.Getenv("BRIDGE_ADMIN_PASSWORD")), nil
}

// SeedSource names which variable supplied the credential, for the
// caller's startup banner.
func SeedSource() string {
	if strings.TrimSpace(os.Getenv("BRIDGE_ADMIN_PASSWORD_FILE")) != "" {
		return "BRIDGE_ADMIN_PASSWORD_FILE"
	}
	return "BRIDGE_ADMIN_PASSWORD"
}

// SetInitialPassword installs an operator-supplied credential into an
// empty store. The MintInitial twin for a password that already exists
// rather than one being generated.
//
// bcrypt runs BEFORE the lock for the same reason MintInitial does: at
// cost 12 it is ~250 ms, and s.mu also guards Verify and
// ValidateSession, so hashing under it would stall all admin auth for
// that window.
func (s *Store) SetInitialPassword(username, password string) error {
	// Runes, not bytes: the message says "characters", and a
	// four-emoji password is 16 bytes and 4 characters. Counting bytes
	// would accept it while the message claims a 12-character floor.
	// (Gemini on PR #802.)
	if utf8.RuneCountInString(password) < minPasswordLen {
		return fmt.Errorf("adminauth: password must be at least %d characters", minPasswordLen)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), adminBcryptCost)
	if err != nil {
		return fmt.Errorf("bcrypt: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.user != nil {
		return ErrAlreadyInitialised
	}
	now := s.now()
	s.user = &userRecord{
		Username:          username,
		PasswordHash:      string(hash),
		CreatedAt:         now,
		PasswordChangedAt: now,
	}
	if err := s.persist(); err != nil {
		// Roll back so an unpersisted credential is never live in
		// memory: the next restart would not have it, and an operator
		// would be locked out by a bridge that had just accepted them.
		s.user = nil
		return err
	}
	return nil
}

// persistSessionsLocked writes the store and clears the dirty flag.
// Caller MUST hold s.mu. No-op when there is no credential to write
// alongside — loopback installs never mint sessions, and persist()
// refuses a nil user.
func (s *Store) persistSessionsLocked(now time.Time) error {
	if s.user == nil {
		s.sessionsDirty = false
		return nil
	}
	if err := s.persist(); err != nil {
		return err
	}
	s.sessionsDirty = false
	s.lastSessionFlush = now
	return nil
}

// sessionExpired reports whether a session has crossed its idle
// timeout or hard cap as of now. Single predicate so CreateSession's
// sweep and ValidateSession's per-lookup check can't drift.
func sessionExpired(sess *Session, now time.Time) bool {
	return now.Sub(sess.IssuedAt) > SessionHardCap ||
		now.Sub(sess.LastUsedAt) > SessionIdleTimeout
}

// sweepExpiredSessionsLocked removes every session past its idle
// timeout or hard cap. Caller MUST hold s.mu.
func (s *Store) sweepExpiredSessionsLocked(now time.Time) {
	for digest, sess := range s.sessions {
		if sessionExpired(sess, now) {
			delete(s.sessions, digest)
		}
	}
}

// evictOldestSessionLocked drops the single least-recently-used
// session (by LastUsedAt). Caller MUST hold s.mu. Called only at the
// maxSessions ceiling; the active operator's most-recently-used
// session is never the eviction target.
func (s *Store) evictOldestSessionLocked() {
	var oldestKey [sha256.Size]byte
	var oldestAt time.Time
	found := false
	for digest, sess := range s.sessions {
		if !found || sess.LastUsedAt.Before(oldestAt) {
			oldestKey = digest
			oldestAt = sess.LastUsedAt
			found = true
		}
	}
	if found {
		delete(s.sessions, oldestKey)
	}
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
	sess, ok := s.sessions[digest]
	if !ok {
		s.mu.Unlock()
		return nil, ErrSessionNotFound
	}
	now := s.now()
	if sessionExpired(sess, now) {
		delete(s.sessions, digest)
		// An expiry is a real state change and worth landing, but it is
		// also self-correcting — load() drops expired sessions anyway —
		// so it rides the debounce rather than forcing a write.
		s.sessionsDirty = true
		s.mu.Unlock()
		return nil, ErrSessionExpired
	}
	sess.LastUsedAt = now
	out := *sess
	// LastUsedAt moves on EVERY authenticated request. Writing each one
	// would put an fsync on the hot path of the entire console, so the
	// bump is debounced; FlushSessions lands the remainder at shutdown.
	s.sessionsDirty = true
	due := now.Sub(s.lastSessionFlush) >= sessionFlushInterval
	if due {
		if err := s.persistSessionsLocked(now); err != nil {
			logger.Error("persist session activity", "err", err)
		}
	}
	s.mu.Unlock()
	return &out, nil
}

// FlushSessions writes any debounced LastUsedAt bumps. Call on clean
// shutdown so the last few minutes of activity are not lost — without
// it a session touched seconds before exit reloads with a stale
// timestamp and expires that much earlier than it should.
//
// Mirrors auth.Store.FlushLastUsed, and is wired from the same place.
func (s *Store) FlushSessions() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sessionsDirty {
		return nil
	}
	return s.persistSessionsLocked(s.now())
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
	if _, ok := s.sessions[digest]; !ok {
		// Unknown token: nothing to revoke, and nothing to write. Logout
		// against an already-expired session must not rewrite the file.
		s.mu.Unlock()
		return
	}
	delete(s.sessions, digest)
	// Synchronous, and this is the one that matters most: a revocation
	// left in the debounce window would be UNDONE by a restart, so a
	// logout would silently not be a logout. Logged at Error for the
	// same reason.
	if err := s.persistSessionsLocked(s.now()); err != nil {
		logger.Error("persist session revocation", "err", err)
	}
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
	// Two shapes. The envelope is current; a bare userRecord is what
	// every install before sessions were persisted has on disk. The
	// discriminator is a top-level "passwordHash", which only the
	// legacy shape has — checked rather than guessed from a failed
	// unmarshal, because encoding/json ignores unknown fields and would
	// happily decode the legacy file into an all-nil envelope.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("parse adminauth store: %w", err)
	}
	if _, legacy := probe["passwordHash"]; legacy {
		var rec userRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("parse adminauth store: %w", err)
		}
		s.user = &rec
		// No sessions to restore, and the next write upgrades the file
		// in place. Nothing to migrate explicitly.
		return nil
	}
	var f storeFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("parse adminauth store: %w", err)
	}
	s.user = f.User
	s.sessions = make(map[[sha256.Size]byte]*Session, len(f.Sessions))
	now := s.now()
	for hexKey, sess := range f.Sessions {
		if sess == nil {
			continue
		}
		// A session already past its deadline is dropped at load rather
		// than resurrected for one request: restoring an expired session
		// would let a restart EXTEND a login, which is the opposite of
		// what the hard cap is for.
		if sessionExpired(sess, now) {
			continue
		}
		digest, err := hex.DecodeString(hexKey)
		if err != nil || len(digest) != sha256.Size {
			// A hand-edited or truncated key cannot address anything;
			// skipping it costs one login.
			continue
		}
		var k [sha256.Size]byte
		copy(k[:], digest)
		s.sessions[k] = sess
	}
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
	f := storeFile{User: s.user, Sessions: make(map[string]*Session, len(s.sessions))}
	for digest, sess := range s.sessions {
		f.Sessions[hex.EncodeToString(digest[:])] = sess
	}
	data, err := json.MarshalIndent(f, "", "  ")
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
	if err := atomicwrite.RenameWithRetry(tmpName, s.path); err != nil {
		return fmt.Errorf("rename adminauth store: %w", err)
	}
	tmpName = "" // success — suppress the cleanup defer
	return nil
}

// passwordAlphabet excludes the most confusable glyphs — the digits
// 0 and 1, uppercase O and I, and lowercase l — so an operator
// transcribing the printed initial password from a terminal banner
// doesn't trip on collisions. Note the exclusion is asymmetric:
// lowercase i and o are KEPT even though they collide with the
// dropped 1/l/I and 0/O groups. That inconsistency is deliberate
// enough to leave alone — changing the alphabet would re-derive the
// entropy / rejection-sampling math below.
const passwordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// rejectionLimit returns the smallest byte value that must be
// REJECTED to keep a `% alphabetLen` draw unbiased: the largest
// multiple of alphabetLen that is ≤ 256. Bytes in [limit, 256) are
// resampled; the returned value is in 1..256, never 0.
//
// **The int return type is load-bearing.** The original form was
//
//	limit := byte(256 - (256 % int(alphabetLen)))
//
// which is correct only while alphabetLen does NOT divide 256: for
// any divisor (1, 2, 4, … 64, 128) the expression is byte(256), and
// byte(256) is 0 — so `b[0] < limit` is never true and the draw loop
// spins forever. With 57 characters that was latent, but the
// docblock on passwordAlphabet explicitly invites editing the
// alphabet, and 64 is about the most natural size anyone would pick.
// The failure mode is the worst kind: `bridge init --public` and
// `bridge admin reset-password` hang with no output and no CPU
// diagnosis, on the one path that mints the operator's credentials.
//
// Keeping the arithmetic in int (and comparing in int at the call
// site) makes the divides-evenly case land on 256 — "reject nothing",
// which is exactly right, since an alphabet that divides 256 has no
// modulo bias to correct.
// **Domain: 1..256.** A return of 0 means "no single-byte draw can serve
// this alphabet" and the caller MUST refuse rather than enter the loop —
// `b[0] < 0` is false for every byte, so a 0 limit spins forever. Two
// inputs produce it: alphabetLen <= 0 (guarded below, since it would also
// divide by zero), and alphabetLen > 256, where `256 % alphabetLen` is
// 256 and the subtraction lands on 0. The latter is not a defect in this
// arithmetic — the largest multiple of 300 that fits in a byte genuinely
// is zero, and an alphabet wider than a byte cannot be sampled from one
// byte without bias anyway. generateFromAlphabet is where that gets
// rejected.
func rejectionLimit(alphabetLen int) int {
	if alphabetLen <= 0 {
		// Not reachable from generatePassword (passwordAlphabet is a
		// non-empty const), but a zero would be a division by zero in
		// the caller's `%`. Return 0 so a caller that ignores this
		// draws nothing rather than panicking or spinning.
		return 0
	}
	return 256 - (256 % alphabetLen)
}

// generatePassword returns a 16-character alphanumeric string
// drawn from passwordAlphabet using crypto/rand. 16 chars from a
// 57-character alphabet ≈ 93 bits of entropy — well above what
// bcrypt's design comfortably handles.
func generatePassword() (string, error) {
	return generateFromAlphabet(passwordAlphabet, 16)
}

// generateFromAlphabet draws length characters uniformly from
// alphabet using crypto/rand.
//
// Uses rejection sampling to eliminate modulo bias (Gemini medium
// review on PR #290). For the 57-character passwordAlphabet,
// 256 % 57 = 28, so a naive `b[0] % 57` would make the first 28
// positions ~25 % (5/4) more likely than the last 29; we discard any
// byte ≥ 228 (= 4 × 57) and resample, an ≈ 11 % rejection rate.
// The limit is derived from len(alphabet) at runtime, so the sampling
// stays unbiased for **any alphabet this function accepts** — see
// rejectionLimit for the arithmetic subtlety that makes that true, and
// for why the accepted range stops at 256.
//
// **An alphabet wider than 256 bytes is REFUSED, not sampled.** One
// byte cannot address more than 256 positions without bias, so there is
// no correct draw to attempt: `rejectionLimit` returns 0 for that range
// and the loop below would accept no byte and spin forever. Refusing is
// the whole fix — the same shape as the byte-overflow this rejection
// limit was rewritten to cure, just approached from above rather than
// from a divisor.
//
// Split out from generatePassword so a test can exercise the sampler
// against alphabet sizes the production const doesn't use.
func generateFromAlphabet(alphabet string, length int) (string, error) {
	alphabetLen := len(alphabet)
	if alphabetLen == 0 || alphabetLen > 256 || length <= 0 {
		return "", errors.New("adminauth: generateFromAlphabet needs an alphabet of 1..256 bytes and a positive length")
	}
	limit := rejectionLimit(alphabetLen)
	out := make([]byte, length)
	for i := 0; i < length; {
		var b [1]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		// Compared in int: a `byte` comparison could not express the
		// "reject nothing" limit of 256.
		if int(b[0]) < limit {
			out[i] = alphabet[int(b[0])%alphabetLen]
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
