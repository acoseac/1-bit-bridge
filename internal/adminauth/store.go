// Package adminauth manages the admin console's single-user
// authentication layer: bcrypt-hashed credentials persisted to disk,
// session tokens persisted beside them, and a per-(IP, username) login
// rate limiter. Gated on `deployment.mode == public` — loopback installs
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
// no new primitive, no new failure mode, and keeps instant revocation:
// deleting a row ends one session in the serving bridge, and a sign-out
// from another process is one field in the same file (see
// SignOutEverywhere).
package adminauth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"golang.org/x/crypto/bcrypt"
)

var logger = logging.Component("adminauth")

// FileName is the credential store's name in the data directory, where
// every command that opens it looks: `bridge serve`, `bridge init` and the
// `bridge admin` family. One definition, since a second spelling that
// drifted would open an empty store beside the real one.
const FileName = "adminauth.json"

// adminBcryptCost is the bcrypt work factor. 12 is a deliberate
// sweet spot (~250 ms on the slowest supported target — Windows
// arm64). Lower would weaken brute-force resistance; higher would
// make the login path feel sluggish on weak hardware. Bump only via
// a deliberate PR with target-host benchmarks.
const adminBcryptCost = 12

// testHashCost, when positive, replaces adminBcryptCost for NEWLY GENERATED
// hashes. It exists only so a test suite stops spending its time in key
// derivation, which is otherwise the single largest cost in CI: bcrypt at cost
// 12 is ~250 ms by design, the race detector multiplies that by roughly eight,
// and this package alone measured 35 s without `-race` against 297 s with it.
//
// Production code must never set it, and nothing enforces that at compile time,
// so TestNoProductionCodeLowersTheHashCost walks every non-test file in the
// module and fails if any of them calls the setter.
//
// Verification is unaffected either way: bcrypt reads the cost from the stored
// hash, so a store written at one cost still verifies at another.
//
// It is atomic rather than a plain variable. The obvious shape is "set it once
// in TestMain and never again", which needs no synchronisation — but two tests
// have to prove the NO-OVERRIDE path, so they set it during the run, and a
// store's own background writers can hash while they do. The repo has been here
// before with sendErrStreak: a field left unsynchronised binds tests too, and
// the failure is an intermittent `-race` report on CI that does not reproduce
// locally. An atomic load beside a ~250 ms key derivation is not a cost worth
// weighing.
var testHashCost atomic.Int64

// SetTestHashCost lowers the bcrypt work factor for the rest of the process.
// Passing 0 restores the shipped cost. Production code must never call it.
func SetTestHashCost(cost int) { testHashCost.Store(int64(cost)) }

// getTestHashCost reads the override. Exported to this package's tests so they
// save and restore it without touching the variable directly.
func getTestHashCost() int { return int(testHashCost.Load()) }

// hashCost is the work factor used for a new hash.
func hashCost() int {
	if c := getTestHashCost(); c > 0 {
		return c
	}
	return adminBcryptCost
}

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
	// ErrStoreUnreadable is a session check that could not read the store
	// file. The file may hold a sign-out this process has not taken, so the
	// session is refused for this request, and kept: the next request reads
	// again, and a sign-out it then finds ends it.
	ErrStoreUnreadable = errors.New("adminauth: the credential store cannot be read")
	// ErrSessionsNotSaved is EndOtherSessions ending sessions that its
	// write then failed to put on disk: they are refused from now on, and a
	// restart before a later write lands the change would sign them back
	// in. The count it returns beside this is still how many it ended.
	ErrSessionsNotSaved = errors.New("adminauth: the sessions were ended but not yet saved")
)

// SessionAction says what a password rotation does to the console
// sessions already signed in.
type SessionAction int

const (
	// EndSessions signs every console out, in this process and, through
	// the file, in a running bridge: `bridge admin reset-password`'s
	// default. The zero value, so a caller that says nothing gets it.
	EndSessions SessionAction = iota
	// KeepSessions leaves them signed in (`--keep-sessions`).
	KeepSessions
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

// storeFile is the on-disk envelope: the credential, the last sign-out,
// and the live sessions. Sessions are keyed by the HEX of the token
// digest — the same value the in-memory map keys on, rendered as a
// string because JSON object keys must be strings. The raw token is
// never written; only its SHA-256, exactly as in memory.
type storeFile struct {
	User *userRecord `json:"user"`
	// SessionsRevokedAt is when every console was last signed out by
	// another process (a rotation, or `bridge admin sign-out-everywhere`).
	// It is how a sign-out reaches a running bridge, which holds its
	// sessions in memory and writes them back (adoptSignOutLocked), so
	// every write carries it over. Absent until the first sign-out.
	SessionsRevokedAt *time.Time          `json:"sessionsRevokedAt,omitempty"`
	Sessions          map[string]*Session `json:"sessions,omitempty"`
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
//
// The mutex is per PROCESS, and the file is not. `bridge serve` holds a
// Store for its whole life while `bridge admin reset-password`, `bridge
// admin login-link` and `bridge init` each open another on the same file,
// so two rules keep one process from undoing another's write:
//
//   - The CREDENTIAL is the file's. Every decision about it (a login, a
//     ticket's account, whether an account exists) and every write of the
//     file re-reads it first (refreshCredentialLocked), and a file that
//     cannot be read decides nothing and is written over by nothing. This
//     process's copy is never ahead of the file, because every credential
//     write here is synchronous and adopted only once it has landed, so
//     "the file's" needs no timestamp to be the newer one.
//   - The SESSIONS are the serving bridge's, the only process that makes
//     them, so a session write puts down the set held in memory. Another
//     process ends them only all at once, through the sign-out marker it
//     writes beside the credential (SignOutEverywhere, and a rotation
//     that does not keep them): the serving bridge takes it at its next
//     read of the file, before anything it writes, and a session check
//     reads the file whenever a stat says it changed. A write that keeps
//     the sessions puts down the set it finds in the file at the write,
//     never the one it read at open.
//
// And no write replaces a file that changed after the read it was built
// from (commitLocked): staging costs a write and an fsync, milliseconds in
// which the other process can commit, so each writer re-reads the file
// just before its rename and rebuilds from the fresh read if it changed.
//
// Before these rules, `persist()` wrote this process's copy of both, so a
// running bridge's next session write (a login, the 30 s activity flush,
// a logout, the shutdown flush) put back the password hash
// reset-password had just rotated away. And with only the first two, no
// process could end a session another one held: a rotation kept them, a
// restart reloaded them, and the running bridge wrote them back beside
// whatever the file held.
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

	// revokedAt is the sign-out marker this process last took from the
	// file (adoptSignOutLocked), and seen the file that read found, which
	// a session check's stat is compared with (refreshIfChangedLocked).
	// lastUnreadableLog throttles that check's line about a file it
	// cannot read. All guarded by mu.
	revokedAt         time.Time
	seen              fileStamp
	lastUnreadableLog time.Time
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

// IsInitialised reports whether the credentials file held an account
// at the last read: the open, or since then a login attempt, a write,
// or a session check that found the file changed. Every caller asks
// straight after OpenStore. Used by the bridge serve startup path to
// refuse-to-start in public mode when no admin has been minted yet.
func (s *Store) IsInitialised() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.user != nil
}

// Username returns the configured admin username, or "" if not
// initialised. Used by the login form to pre-fill the field
// (single-user system, no risk in exposing the username).
//
// It answers from the last read rather than reading the file, since
// the form is served to anyone who asks. A pre-fill that lags an
// account replaced elsewhere costs one failed login, and that login
// attempt is itself a read.
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
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), hashCost())
	if err != nil {
		return "", fmt.Errorf("bcrypt: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.installInitialLocked(username, string(hash)); err != nil {
		return "", err
	}
	return plaintext, nil
}

// installInitialLocked writes the store's first credential. Whether there
// already is one is the FILE's answer at the write, not this process's
// copy: two processes that both opened an empty store (`bridge init` and a
// bridge seeding from its environment, say) would otherwise both mint, and
// the second would replace the password the first one printed. The file's
// sessions are carried over as they are. Caller holds s.mu.
//
// The credential is adopted only once the write has landed, so a failed
// write leaves this process with no credential, as the file has none, and
// never one the next restart would not have.
func (s *Store) installInitialLocked(username, hash string) error {
	next, _, err := s.commitLocked(func(cur storeContents) (storeFile, error) {
		if cur.user != nil {
			return storeFile{}, ErrAlreadyInitialised
		}
		now := s.now()
		return storeFile{
			User: &userRecord{
				Username:          username,
				PasswordHash:      hash,
				CreatedAt:         now,
				PasswordChangedAt: now,
			},
			SessionsRevokedAt: cur.signOut(),
			Sessions:          cur.sessions,
		}, nil
	})
	if err != nil {
		return err
	}
	s.user = next.User
	return nil
}

// ResetPassword overwrites the existing credentials with a new
// bcrypt hash. Called by `bridge admin reset-password`, usually in a
// process of its own beside a running bridge, which re-reads the file
// at its next login attempt and at every write, so the rotation takes
// there with no restart.
//
// With EndSessions, the default, it also signs every console out, in the
// same write: see SignOutEverywhere for how that reaches a running
// bridge. The rotation is the moment an operator whose password leaked
// runs this, and a session signed in with the leaked password is the
// thing left to end. With KeepSessions it writes back the set it finds
// in the file, and nothing ends them: not a restart, since they persist
// in this same file (PR #800). Until 2026-09-27 that was the only
// behaviour, pinned as operator-friendly, while 1-bit.app's
// troubleshooting page told operators that a reset invalidated them
// immediately. Either way the write is confirmed against a running
// bridge's write in flight at its rename (commitAndConfirm), which would
// put back the credential this replaced.
func (s *Store) ResetPassword(username, newPassword string, sessions SessionAction) error {
	if newPassword == "" {
		return errors.New("adminauth: new password must not be empty")
	}
	// Hash BEFORE taking s.mu. bcrypt at cost 12 is ~250ms, and s.mu also
	// guards Verify / ValidateSession, so hashing under the lock stalls
	// all admin auth for that window. bcrypt needs only newPassword, no
	// store state. A username-mismatch caller wastes the hash on the
	// error path (rare) — acceptable for keeping the success path off the
	// lock.
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), hashCost())
	if err != nil {
		return fmt.Errorf("bcrypt: %w", err)
	}
	// Decide from the file, and when keeping the sessions write back the
	// ones IT holds: the running bridge signs sessions in and out while
	// this process waits at its password prompt, and the copy read at open
	// would drop the new ones and put back the ones signed out.
	// commitLocked makes that the file as it is at the rename, not only at
	// the read: a login or logout committed while this write was staging
	// would otherwise be overwritten, and a logout overwritten that way
	// comes back at the next restart (CodeRabbit on #1039).
	build := func(cur storeContents) (storeFile, error) {
		if cur.user != nil && cur.user.Username != username {
			return storeFile{}, ErrUsernameMismatch
		}
		now := s.now()
		// A FRESH userRecord pointer, never a mutation in place (CodeRabbit
		// Critical + Major review post-PR-#292). Verify takes `user :=
		// s.user` under the lock and releases it for the slow bcrypt
		// compare, so a mutation would race that read. And it is swapped in
		// only once the write has landed, so a failed write leaves this
		// process on the credential the file still holds, where an early
		// swap let the new password log in until the next restart reverted
		// it.
		rec := &userRecord{
			Username:          username,
			PasswordHash:      string(hash),
			CreatedAt:         now,
			PasswordChangedAt: now,
		}
		if cur.user != nil {
			rec.CreatedAt = cur.user.CreatedAt
		}
		if sessions == KeepSessions {
			return storeFile{User: rec, SessionsRevokedAt: cur.signOut(), Sessions: cur.sessions}, nil
		}
		return storeFile{User: rec, SessionsRevokedAt: nextSignOut(now, cur.revokedAt)}, nil
	}
	took := func(next storeFile) {
		s.user = next.User
		if sessions != KeepSessions {
			s.tookOwnSignOutLocked(*next.SessionsRevokedAt)
		}
	}
	// A running bridge's write in flight at the rename carries the
	// credential this replaced, and with it the marker: see
	// commitAndConfirm.
	reverted := func(replaced, cur storeContents) bool {
		return replaced.user != nil && sameCredential(cur.user, replaced.user)
	}
	return s.commitAndConfirm(build, took, reverted)
}

// SignOutEverywhere ends every console session, with the password left as
// it is: `bridge admin sign-out-everywhere`, for a session that must end
// while the password need not change (a cookie left on a shared machine,
// or a rotation run with --keep-sessions).
//
// It runs in a process of its own beside the running bridge, which holds
// the sessions in memory and writes them back, so emptying the file's set
// is not enough: the bridge's next write would put its set back. The
// write therefore also moves the file's sign-out marker, and the running
// bridge ends every session it holds when it reads a marker it has not
// seen (adoptSignOutLocked). It reads the file before every write, and a
// session check reads it whenever a stat says the file changed, so the
// sessions end at their next request, with no restart. A restart does
// not bring them back, since the file no longer holds them. The write is
// confirmed against a running bridge's write in flight at its rename
// (commitAndConfirm), which would put back the marker this replaced.
//
// ErrNotInitialised when the file holds no credential: the marker is
// written beside one, and a store without one serves no console.
func (s *Store) SignOutEverywhere() error {
	build := func(cur storeContents) (storeFile, error) {
		if cur.user == nil {
			return storeFile{}, ErrNotInitialised
		}
		return storeFile{User: cur.user, SessionsRevokedAt: nextSignOut(s.now(), cur.revokedAt)}, nil
	}
	took := func(next storeFile) { s.tookOwnSignOutLocked(*next.SessionsRevokedAt) }
	// A running bridge's write in flight at the rename carries the marker
	// this replaced: see commitAndConfirm.
	reverted := func(replaced, cur storeContents) bool {
		return cur.user != nil && cur.revokedAt.Equal(replaced.revokedAt)
	}
	return s.commitAndConfirm(build, took, reverted)
}

// commitAndConfirm commits build's file and takes it into this process
// (took, under s.mu), then confirms it: after confirmSettle, with s.mu
// released, it reads the file again, and when reverted says the file is
// back to what the commit replaced it commits once more, up to
// maxConfirmRedos times.
//
// The confirmation closes the one window commitLocked's check cannot:
// a write the running bridge had checked (its own unchangedSince) but not
// yet renamed when this commit renamed. That write was built from the file
// before this one, so it carries back exactly the credential and the
// sign-out marker this replaced, and once it lands the bridge never sees
// the marker and its later writes carry the old state on. It lands within
// RenameWithRetry's retry budget of its check (750 ms on Windows, where
// a scanner holding a fresh file forces the retries; no retry on POSIX),
// and its check came before this rename, so after confirmSettle it has
// landed or never will. A newer write by another process carries a newer
// credential or marker, which reverted does not match, and is left alone.
// Only a writer stalled for longer than the settle between its check and
// its rename gets past this, which a kernel lock would close (declined in
// #1039, for its new failure modes in the bridge's own write path; see
// ops/engineering-log.md, #1044). CodeRabbit on #1044.
//
// The write already landed, so a confirmation read that fails is not an
// error; it is logged, since nothing read shows the write held either.
func (s *Store) commitAndConfirm(
	build func(cur storeContents) (storeFile, error),
	took func(next storeFile),
	reverted func(replaced, cur storeContents) bool,
) error {
	s.mu.Lock()
	next, replaced, err := s.commitLocked(build)
	if err == nil {
		took(next)
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	for redo := 0; ; redo++ {
		time.Sleep(confirmSettle)
		if beforeConfirmHook != nil {
			beforeConfirmHook()
		}
		s.mu.Lock()
		cur, err := readStoreFile(s.path)
		if err != nil {
			s.mu.Unlock()
			// The write landed, so this is no failure; but nothing read
			// shows it held either (CodeRabbit on #1044).
			logger.Warn(msgUnconfirmedLog, "path", s.path, "err", err)
			return nil
		}
		if !reverted(replaced, cur) {
			s.mu.Unlock()
			return nil
		}
		if redo == maxConfirmRedos {
			s.mu.Unlock()
			return errWrittenOver
		}
		next, _, err = s.commitLocked(build)
		if err == nil {
			took(next)
		}
		s.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

// confirmSettle is how long commitAndConfirm waits before its read: the
// rename retry budget, in which a checked write lands, and a margin. A var
// so this package's tests can run without the wait.
var confirmSettle = atomicwrite.RenameRetryBudget() + 250*time.Millisecond

// maxConfirmRedos bounds how often commitAndConfirm commits again for a
// file that keeps coming back: one stale write is the race, three are a
// writer that is not converging, reported rather than chased.
const maxConfirmRedos = 3

// errWrittenOver is commitAndConfirm giving up on a file that kept going
// back to what it replaced.
var errWrittenOver = errors.New("adminauth: another process kept writing the store back over this change; run the command again")

// beforeConfirmHook is a test-only seam (nil in production), fired before
// each confirmation read: where a test lands the running bridge's
// in-flight write.
var beforeConfirmHook func()

// msgUnconfirmedLog is commitAndConfirm's line for a confirmation read that
// failed.
const msgUnconfirmedLog = "wrote the admin credential store, but could not read it back to confirm a running bridge did not write over it; run the command again if it did not take"

// nextSignOut is the marker a sign-out writes: now, or one nanosecond past
// the marker the file already holds when the clock does not put now after
// it. Every sign-out must CHANGE the marker, because a process that has
// taken the last one ignores it: a clock stepped back, or two sign-outs
// inside one tick of a coarse clock (15.6 ms on Windows), would otherwise
// write a marker a running bridge has seen, and end nothing there.
func nextSignOut(now, last time.Time) *time.Time {
	at := now.Round(0) // no monotonic reading: the marker is compared with ones read back from the file
	if !last.IsZero() && !at.After(last) {
		at = last.Add(time.Nanosecond)
	}
	return &at
}

// tookOwnSignOutLocked is this process taking the sign-out it has just
// written: its own sessions end, and the marker is one it has seen, so
// reading it back ends nothing it holds by then. No log line, unlike a
// sign-out taken from another process: the command that asked prints its
// own. Caller MUST hold s.mu.
func (s *Store) tookOwnSignOutLocked(at time.Time) {
	s.revokedAt = at
	clear(s.sessions)
	s.sessionsDirty = false
}

// Verify checks the credentials and returns nil on match.
// ErrInvalidCredentials covers both wrong-user and wrong-password
// cases — never disclose which one failed to the caller (handler
// reports a single generic "invalid credentials" to the wire).
// Constant-time username compare is unnecessary (username is not
// secret); bcrypt.CompareHashAndPassword is constant-time on the
// hash side.
//
// It checks against the credential in the FILE, re-read for every
// attempt, so a rotation made by another process takes at this
// process's next login. A file it cannot read is a refusal: this
// process's copy is the credential as it was, which after a rotation
// is the one rotated away from. The read is small beside the bcrypt
// compare that follows it, and a login attempt is already rate-limited.
func (s *Store) Verify(username, password string) error {
	s.mu.Lock()
	err := s.refreshCredentialLocked()
	user := s.user
	s.mu.Unlock()
	if err != nil {
		logger.Error("could not read the admin credential; refusing the login", "err", err)
		return err
	}
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
	// this process and works, and it stays pending (the dirty flag), so a
	// later activity write or the shutdown flush lands it. Refusing a
	// valid login because the disk hiccuped would be the worse trade.
	s.sessionsDirty = true
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
	hash, err := bcrypt.GenerateFromPassword([]byte(password), hashCost())
	if err != nil {
		return fmt.Errorf("bcrypt: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// installInitialLocked adopts the credential only once it is on disk,
	// so an unpersisted one is never live in memory: the next restart
	// would not have it, and an operator would be locked out by a bridge
	// that had just accepted them.
	return s.installInitialLocked(username, string(hash))
}

// persistSessionsLocked writes this process's sessions beside the
// credential the FILE holds, and clears the dirty flag once they are
// down. Caller MUST hold s.mu. Every session write goes through here:
// a login, the debounced activity write, a logout, the shutdown flush.
//
// The credential is re-read first and never taken from memory: writing
// this process's copy is how a running bridge put a rotated-away
// password back on disk. So is a sign-out, which ends the sessions it
// finds before the set to write is built from what is left, and whose
// marker the write carries over. The write commits only if the file is
// still the one it was built from (commitLocked), because the staging's
// fsync is a window of milliseconds, and a rotation or sign-out landing in
// it is rebuilt around. A file that cannot be read is not written over,
// since it may hold a credential newer than this process has seen; the
// change stays pending (dirty) for the next attempt.
//
// Every attempt starts the next debounce window, a failed one too.
// Otherwise every authenticated request after a failure is "due", and
// a write that keeps failing costs an attempt and an error line per
// console request.
//
// Nothing is written when the file holds no credential to write beside
// (a store that was never initialised, or a file that is gone, which a
// session write must not recreate with the credential it replaced), and
// the sessions stay pending: they are this process's, and go down
// beside whichever credential the file holds next.
func (s *Store) persistSessionsLocked(now time.Time) error {
	s.lastSessionFlush = now
	written, _, err := s.commitLocked(func(cur storeContents) (storeFile, error) {
		// The set is built HERE, from memory as it is after commitLocked
		// took the read: a sign-out found in it has just ended sessions this
		// process held, and a set built before the read would write them
		// back into the file the sign-out emptied.
		sessions := make(map[string]*Session, len(s.sessions))
		for digest, sess := range s.sessions {
			sessions[hex.EncodeToString(digest[:])] = sess
		}
		// The file's credential and sign-out, never this process's copy.
		return storeFile{User: cur.user, SessionsRevokedAt: cur.signOut(), Sessions: sessions}, nil
	})
	if err != nil {
		return fmt.Errorf("sessions not written: %w", err)
	}
	if written.User == nil {
		// Still pending: these sessions are this process's, so they go
		// down beside whichever credential the file holds next.
		return nil
	}
	s.sessionsDirty = false
	return nil
}

// commitLocked writes the file build returns, built from the file as it is
// now, and commits it only if the file is still, byte for byte, the one it
// was built from. Every write of the store goes through here. Caller MUST
// hold s.mu.
//
// Staging costs a write and an fsync, milliseconds (tens on a cloud disk)
// in which another process can commit. Renaming over that commit would
// undo it: a rotation put back by the running bridge's session write, or a
// login or logout the bridge committed dropped by reset-password's. So the
// file is read once more just before the rename, and a change starts the
// write again from a fresh read, up to maxCommitAttempts. What remains is
// the rename itself, and on Windows its retries (a kernel lock would close
// that and is not taken here; see ops/engineering-log.md, #1039).
//
// Each read is taken into this process first (adoptLocked), so a build
// decides from the file's credential and from memory as a sign-out in the
// file has left it. build returns the file to write, or an error that
// ends the write; a file with no User writes nothing and returns a zero
// storeFile and a nil error. A read that fails writes nothing and returns
// its error. It also returns the contents the written file was built from,
// which commitAndConfirm compares a later read with.
//
// A commit records the stamp of the file it put in place, so the session
// check's stat gate does not read back what this process just wrote
// (Gemini's review, 2026-09-27): each session write would otherwise cost
// the next console request a full read.
func (s *Store) commitLocked(build func(cur storeContents) (storeFile, error)) (storeFile, storeContents, error) {
	for attempt := 1; ; attempt++ {
		cur, err := readStoreFile(s.path)
		if err != nil {
			return storeFile{}, storeContents{}, err
		}
		s.adoptLocked(cur)
		next, err := build(cur)
		if err != nil || next.User == nil {
			return storeFile{}, storeContents{}, err
		}
		written, err := s.writeStoreLocked(next, s.unchangedSince(cur))
		if errors.Is(err, errStoreMoved) && attempt < maxCommitAttempts {
			continue
		}
		if err != nil {
			return storeFile{}, storeContents{}, err
		}
		if written != nil {
			s.seen = fileStamp{known: true, info: written}
		}
		return next, cur, nil
	}
}

// unchangedSince is the beforeCommit of a write built from read: it lets
// the staged file replace the store only while the store is still, byte
// for byte, the file read was taken from, and answers errStoreMoved
// otherwise. A missing file reads as nil, as it does in read. Split out of
// commitLocked for SonarCloud's go:S3776 (cognitive complexity) on #1039.
func (s *Store) unchangedSince(read storeContents) func() error {
	return func() error {
		if beforeCommitHook != nil {
			beforeCommitHook()
		}
		latest, err := os.ReadFile(s.path)
		if errors.Is(err, os.ErrNotExist) {
			latest, err = nil, nil
		}
		if err != nil {
			return fmt.Errorf("re-read adminauth store before the commit: %w", err)
		}
		if !bytes.Equal(latest, read.raw) {
			return errStoreMoved
		}
		return nil
	}
}

// errStoreMoved is a write finding the file changed between the read it
// was built from and its commit.
var errStoreMoved = errors.New("adminauth: the store file changed while it was being written; nothing written")

// maxCommitAttempts bounds how often a write rebuilds for a file that
// keeps changing under it. Each attempt costs an fsync, and a file that
// changes three times inside three of them is not a rotation anyone is
// running by hand. A session write stays pending; a credential write
// fails.
const maxCommitAttempts = 3

// beforeCommitHook is a test-only seam (nil in production), fired in
// every write between the staging of the file and the re-read that gates
// its commit: the window another process's commit lands in. Same
// convention as auth's beforeValidatePersistHook.
var beforeCommitHook func()

// sameCredential reports whether two reads of the file hold the same
// credential. Username and hash are enough: bcrypt salts every hash,
// so a rotation always changes it, even to the same password.
func sameCredential(a, b *userRecord) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Username == b.Username && a.PasswordHash == b.PasswordHash
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
//
// Before the lookup it takes a sign-out another process wrote into the
// file, whenever a stat says the file is not the one last read
// (refreshIfChangedLocked), so a signed-out console is refused at its
// next request rather than at this process's next write. A file it cannot
// read is ErrStoreUnreadable: the session is refused and kept.
func (s *Store) ValidateSession(raw string) (*Session, error) {
	if raw == "" {
		return nil, ErrSessionNotFound
	}
	digest := sha256.Sum256([]byte(raw))
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if err := s.refreshIfChangedLocked(); err != nil {
		s.logUnreadableLocked(now, err)
		return nil, err
	}
	sess, ok := s.sessions[digest]
	if !ok {
		return nil, ErrSessionNotFound
	}
	if sessionExpired(sess, now) {
		delete(s.sessions, digest)
		// An expiry is a real state change and worth landing, but it is
		// also self-correcting — load() drops expired sessions anyway —
		// so it rides the debounce rather than forcing a write.
		s.sessionsDirty = true
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
	return &out, nil
}

// refreshIfChangedLocked reads the store again when a stat says the file
// is not the one this process last read, and takes what it finds
// (adoptLocked): a sign-out another process wrote, and the credential.
// Caller MUST hold s.mu.
//
// It runs on every console request, so an unchanged file must cost little,
// and it does: a stat, where a read and parse grows with the session count
// (measured on darwin/arm64: 2 µs against 22 µs at one session and 1.75 ms
// at maxSessions). The stat misses only a rewrite that keeps the file's
// identity, size and modification time together (fileStamp). A session
// write reads the file in full whatever the stat said, and one comes at
// least every sessionFlushInterval while requests do.
//
// An error, the stat's or the read's, is ErrStoreUnreadable, and nothing
// is recorded, so the next request tries again. Nor is a failure
// remembered against the stamp: a chmod or chown that makes the file
// readable again changes none of the three.
func (s *Store) refreshIfChangedLocked() error {
	fi, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		fi, err = nil, nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStoreUnreadable, err)
	}
	if s.seen.matches(fi) {
		return nil
	}
	if readStoreHook != nil {
		readStoreHook()
	}
	cur, err := readStoreFile(s.path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStoreUnreadable, err)
	}
	s.adoptLocked(cur)
	return nil
}

// readStoreHook is a test-only seam (nil in production), fired each time
// a session check decides to read the file: the count a test of the stat
// gate asserts on.
var readStoreHook func()

// logUnreadableLocked logs a session check that could not read the store,
// at most once a sessionFlushInterval. Every console request makes one
// while the file stays unreadable, and a line each would bury the journal
// (the debounce's reason, TestAFailedSessionWriteWaitsOutTheDebounce); the
// admin middleware leaves the line to this. A clock stepped back logs at
// once rather than going quiet until it catches up. Caller MUST hold s.mu.
func (s *Store) logUnreadableLocked(now time.Time, err error) {
	if since := now.Sub(s.lastUnreadableLog); !s.lastUnreadableLog.IsZero() && since >= 0 && since < sessionFlushInterval {
		return
	}
	s.lastUnreadableLog = now
	logger.Error(msgStoreUnreadableLog, "path", s.path, "err", err)
}

// msgStoreUnreadableLog is logUnreadableLocked's line.
const msgStoreUnreadableLog = "could not read the admin credential store; refusing console sessions until it reads"

// EndOtherSessions ends every console session but the one raw names, and
// returns how many it ended: the console's "Sign out all other sessions".
// ErrSessionNotFound when raw names no live session, and nothing is ended.
//
// It runs in the serving bridge, which holds the sessions, so it needs no
// marker: it deletes them from memory and writes the set that is left at
// once, as a logout does, because an end only in memory comes back at the
// next restart. A write that fails stays pending, as a logout's does, but
// unlike a logout it is REPORTED (ErrSessionsNotSaved, with the count): an
// operator signing a stranger out needs to know that a restart before the
// next write would sign the stranger back in. Not a plain failure either,
// since the sessions are already refused (Gemini on #1044 proposed
// returning the write's error, which a caller would report as "could not
// sign out"). A sign-out another process wrote is taken first
// (ErrStoreUnreadable when the file cannot be read, as ValidateSession).
func (s *Store) EndOtherSessions(raw string) (int, error) {
	if raw == "" {
		return 0, ErrSessionNotFound
	}
	keep := sha256.Sum256([]byte(raw))
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if err := s.refreshIfChangedLocked(); err != nil {
		s.logUnreadableLocked(now, err)
		return 0, err
	}
	// Expired sessions are no one's to sign out, and counting them would
	// report browsers that had long been signed out already.
	s.sweepExpiredSessionsLocked(now)
	if _, ok := s.sessions[keep]; !ok {
		return 0, ErrSessionNotFound
	}
	ended := 0
	for digest := range s.sessions {
		if digest != keep {
			delete(s.sessions, digest)
			ended++
		}
	}
	if ended == 0 {
		return 0, nil
	}
	s.sessionsDirty = true
	if err := s.persistSessionsLocked(now); err != nil {
		return ended, fmt.Errorf("%w: %w", ErrSessionsNotSaved, err)
	}
	return ended, nil
}

// LiveSessionCount returns how many console sessions are signed in and
// not yet past a deadline: the Devices page's count.
func (s *Store) LiveSessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := 0
	for _, sess := range s.sessions {
		if !sessionExpired(sess, now) {
			n++
		}
	}
	return n
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
	// same reason, and marked pending first, so a write that fails is
	// retried by the next one and at the latest by the shutdown flush.
	// Before, a failed write left nothing pending, and with no other
	// session active the flush found nothing to land.
	s.sessionsDirty = true
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
	c, err := readStoreFile(s.path)
	if err != nil {
		return err
	}
	s.user = c.user
	// The marker is taken as SEEN, and the file's sessions all kept: every
	// writer that leaves the marker in place puts down only sessions made
	// after it was taken (adoptSignOutLocked), so none of them predates it.
	s.revokedAt = c.revokedAt
	s.seen = c.stamp
	s.sessions = make(map[[sha256.Size]byte]*Session, len(c.sessions))
	now := s.now()
	for hexKey, sess := range c.sessions {
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
	// Start the debounce window now. Without this lastSessionFlush is
	// the zero time, so the very FIRST authenticated request after
	// startup is always "due" and writes synchronously — a guaranteed
	// fsync on the first page load of every boot, to persist timestamps
	// that were just read off disk unchanged. (Gemini on PR #800.)
	s.lastSessionFlush = now
	return nil
}

// storeContents is what one read of the store file found. sessions is
// keyed by the hex digest and holds exactly what the file held,
// expired and malformed entries included, so a credential write can
// carry them over untouched. raw is the file's bytes, nil for a file
// that is missing, which commitLocked compares before a rename. revokedAt
// is the sign-out marker, zero when the file has none, and stamp the
// stat of the file read.
type storeContents struct {
	raw       []byte
	stamp     fileStamp
	user      *userRecord
	revokedAt time.Time
	sessions  map[string]*Session
}

// signOut is the file's sign-out marker as a writer carries it over, nil
// for none.
func (c storeContents) signOut() *time.Time {
	if c.revokedAt.IsZero() {
		return nil
	}
	at := c.revokedAt
	return &at
}

// fileStamp is what a stat says about the store file a read found: enough
// to tell, with another stat, that the file has been replaced or rewritten
// since, without reading it (refreshIfChangedLocked).
type fileStamp struct {
	known bool        // false before any read: no stat matches it
	info  os.FileInfo // nil for a file that was not there
}

// matches reports whether fi, a stat taken now (nil for a file that is not
// there), describes the file the stamp was taken of, unchanged.
//
// os.SameFile is the term every write here trips: each one renames a new
// file into place, and a new file is a new inode (file ID on Windows)
// whatever its size and time. Size and modification time catch a rewrite
// in place, which keeps the inode: a hand edit, a `cp` over the file. What
// all three miss is an in-place rewrite of the same size within one tick
// of the filesystem's clock, or two renames between two stats that
// recycle the first file's inode.
func (st fileStamp) matches(fi os.FileInfo) bool {
	if !st.known {
		return false
	}
	if st.info == nil || fi == nil {
		return st.info == nil && fi == nil
	}
	return st.info.Size() == fi.Size() && st.info.ModTime().Equal(fi.ModTime()) && os.SameFile(st.info, fi)
}

// readStoreFile reads and parses the store at path. A missing or empty
// file is an empty store, not an error. Anything else that stops the
// read or the parse is an error, and a caller holding one must neither
// decide from the file nor write over it: it may hold a credential newer
// than any this process has seen.
//
// The stamp is the stat of the file OPENED, taken before its bytes are
// read, so it describes the file the bytes came from: every writer
// replaces the file by rename, never in place. A stat of the path taken
// after the read could describe a file renamed in between, whose change
// the gate would then never see.
func readStoreFile(path string) (storeContents, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return storeContents{stamp: fileStamp{known: true}}, nil
	}
	if err != nil {
		return storeContents{}, fmt.Errorf("read adminauth store: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return storeContents{}, fmt.Errorf("read adminauth store: %w", err)
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return storeContents{}, fmt.Errorf("read adminauth store: %w", err)
	}
	stamp := fileStamp{known: true, info: info}
	if len(raw) == 0 {
		return storeContents{raw: raw, stamp: stamp}, nil
	}
	// Two shapes. The envelope is current; a bare userRecord is what
	// every install before sessions were persisted has on disk. The
	// discriminator is a top-level "passwordHash", which only the
	// legacy shape has — checked rather than guessed from a failed
	// unmarshal, because encoding/json ignores unknown fields and would
	// happily decode the legacy file into an all-nil envelope.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return storeContents{}, fmt.Errorf("parse adminauth store: %w", err)
	}
	if _, legacy := probe["passwordHash"]; legacy {
		var rec userRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return storeContents{}, fmt.Errorf("parse adminauth store: %w", err)
		}
		// No sessions to restore, and the next write upgrades the file
		// in place. Nothing to migrate explicitly.
		return storeContents{raw: raw, stamp: stamp, user: &rec}, nil
	}
	var sf storeFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return storeContents{}, fmt.Errorf("parse adminauth store: %w", err)
	}
	c := storeContents{raw: raw, stamp: stamp, user: sf.User, sessions: sf.Sessions}
	if sf.SessionsRevokedAt != nil {
		c.revokedAt = *sf.SessionsRevokedAt
	}
	return c, nil
}

// refreshCredentialLocked re-reads the file and takes what it holds
// (adoptLocked): the credential, whatever this process held (see the
// Store docblock for why the file's is always the one to use), and a
// sign-out. On an error nothing changes, and the caller must neither
// decide from the credential nor write the file. Caller MUST hold s.mu.
func (s *Store) refreshCredentialLocked() error {
	c, err := readStoreFile(s.path)
	if err != nil {
		return err
	}
	s.adoptLocked(c)
	return nil
}

// adoptLocked takes a read of the file into this process: the credential,
// a sign-out another process wrote, and the stamp a session check's stat
// is compared with. Every read that decides or writes goes through here,
// so none of them can build on memory the file has overruled. Caller MUST
// hold s.mu.
func (s *Store) adoptLocked(c storeContents) {
	s.adoptCredentialLocked(c.user)
	s.adoptSignOutLocked(c.revokedAt)
	s.seen = c.stamp
}

// adoptSignOutLocked ends every session this process holds when the file
// records a sign-out it has not taken (at is not the marker it last
// took), and takes the marker. Caller MUST hold s.mu.
//
// The marker is an EVENT, not a filter: it is never compared with a
// session's IssuedAt. Every session held when a new marker is read
// predates the sign-out here, one made after the marker's time included,
// since its login read the file before the marker landed and was checked
// against the credential as it was. One made after the read is kept, so a
// clock stepped back cannot end it, as a comparison would.
//
// A file with no marker, or no file, changes nothing, and leaves the
// marker taken as it was: only a sign-out ends sessions, and the same
// marker read back later (a restored copy of the file) is still one this
// process has taken.
func (s *Store) adoptSignOutLocked(at time.Time) {
	if at.IsZero() || at.Equal(s.revokedAt) {
		return
	}
	s.revokedAt = at
	n := len(s.sessions)
	clear(s.sessions)
	logger.Info("the admin credential store records a sign-out everywhere; ending the console sessions this bridge held",
		"sessions", n)
}

// adoptCredentialLocked makes u the credential this process decides
// from, and logs when that is a change another process made, which is
// the line that tells an operator the running bridge took a rotation.
// Caller MUST hold s.mu.
//
// Nothing from the record but the username reaches the log. The line's
// own timestamp says when this process took the change, and CodeQL's
// clear-text-logging query reads any field named for a password as a
// secret, PasswordChangedAt included (#1039).
func (s *Store) adoptCredentialLocked(u *userRecord) {
	switch {
	case u == nil && s.user != nil:
		logger.Warn("the admin credential file is gone; logins are refused until one is set",
			"path", s.path)
	case u != nil && !sameCredential(u, s.user):
		logger.Info("the admin credential changed on disk; using it from now on",
			"username", u.Username)
	}
	s.user = u
}

// writeStoreLocked atomically replaces the credentials file with f.
// 0o700 dir + 0o600 file, same hardening as auth.Store. Caller MUST hold
// the mutex, and decides what f holds: its own sessions for a session
// write, the file's for a write that keeps them, none for a sign-out; and
// the file's sign-out marker unless it is writing a new one.
//
// beforeCommit, when not nil, runs once the new file is staged (written,
// synced and closed) and before it replaces the old one; an error from
// it abandons the write and is returned.
//
// It returns the stat of the file it renamed into place, taken once its
// bytes were synced (the rename keeps the inode, the size and the
// modification time), or nil when that stat failed. commitLocked records
// it for the session check's stat gate.
func (s *Store) writeStoreLocked(f storeFile, beforeCommit func() error) (os.FileInfo, error) {
	if f.User == nil {
		return nil, errors.New("adminauth: cannot persist nil user record")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir adminauth store: %w", err)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".adminauth-*.json")
	if err != nil {
		return nil, fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("chmod tmp: %w", err)
	}
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	defer func() { _ = tmp.Close() }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("sync tmp: %w", err)
	}
	// A failed stat costs the gate one read, never the write.
	staged, statErr := tmp.Stat()
	if statErr != nil {
		staged = nil
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close tmp: %w", err)
	}
	if beforeCommit != nil {
		if err := beforeCommit(); err != nil {
			return nil, err
		}
	}
	if err := atomicwrite.RenameWithRetry(tmpName, s.path); err != nil {
		return nil, fmt.Errorf("rename adminauth store: %w", err)
	}
	tmpName = "" // success — suppress the cleanup defer
	return staged, nil
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
