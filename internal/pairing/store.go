// Package pairing implements the in-memory state machine for the
// admin-approval pairing flow.
//
// The flow:
//
//  1. iOS POSTs to /v1/pairing/requests with a deviceName + the SHA-256
//     hash (hex) of a 32-byte pollSecret it keeps in memory. The bridge
//     creates a Request in state Pending, stores the hash + the bridge's
//     current cert fingerprint, and returns {requestID, verificationCode,
//     ttlSeconds}.
//
//  2. The admin web console lists pending requests, shows the
//     verificationCode (which the operator reads off the iOS device's
//     waiting screen), and Approves/Declines. Approve calls the injected
//     MintFunc to create a real bearer token in the auth store; the raw
//     token is held against the request until the iOS device acknowledges
//     receipt.
//
//  3. iOS polls GET /v1/pairing/{id} with Authorization: Bearer
//     <pollSecret>. The Store SHA-256s the presented secret and
//     constant-time-compares against the stored hash. While Pending the
//     poll returns {status:"pending"}; once Approved the poll returns the
//     raw token on every authorized request — NOT read-once. iOS may
//     legitimately retry the same poll across a network blip, and the
//     pollSecret + cert pin gate the re-reads. The token is removed only
//     when iOS sends DELETE /v1/pairing/{id} (acknowledgment after
//     keychain persist) OR when TTL+grace elapses without acknowledgment,
//     in which case the Store revokes the minted token via the injected
//     RevokeFunc and deletes the row.
//
// State transitions are guarded by a single mutex and a per-request
// timer. Every transition explicitly Stops the prior timer to prevent
// goroutine accumulation under join-spam (only Pending counts toward the
// MaxPending cap; terminal-state rows linger for the grace window so a
// late poll sees the verdict instead of 404).
//
// The Store deliberately holds no persistence — pending requests are
// ephemeral by design. A bridge restart drops every in-flight request;
// iOS detects the restart via bridgeStartedAt mismatch and prompts the
// user to retry. This keeps the disk-leak surface for pollSecret /
// rawToken at zero.
package pairing

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging"
)

var logger = logging.Component("pairing")

// Defaults match the values documented in the iOS-side join-flow plan.
const (
	// DefaultTTL is how long a Pending request stays alive without an
	// admin verdict. After expiry the row transitions to Expired and
	// holds for DefaultGrace before deletion. iOS sees expired requests
	// as terminal and re-taps Join to retry.
	DefaultTTL = 5 * time.Minute

	// DefaultGrace is how long a terminal-state request lingers before
	// deletion, so a poll fired concurrently with the transition still
	// gets a structured verdict instead of a 404.
	DefaultGrace = 60 * time.Second

	// DefaultMaxPending caps concurrent Pending requests bridge-wide.
	// No per-IP cap — under double-NAT/mesh routers every LAN device
	// presents the same router IP, so per-IP throttling produces false
	// positives. The admin UI's visible queue is the single bound;
	// a malicious flood is its own remediation prompt.
	DefaultMaxPending = 16

	// requestIDBytes is the random-byte length of a request ID; emitted
	// as 2x hex chars (12 hex chars total). 48 bits is plenty against
	// brute-force ID enumeration over the TTL window — the polling
	// endpoint authenticates by pollSecret (256-bit), not by ID.
	requestIDBytes = 6
)

// State enumerates the lifecycle of a pairing request. Pending is the
// only mutable state from outside the Store; everything else is set by
// transition methods and lingers for the grace window before deletion.
type State int

const (
	StatePending State = iota
	StateApproved
	StateDeclined
	StateExpired
	StateCertRotated
)

// String returns the lowercase wire form. Stable — iOS decodes by exact match.
func (s State) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateApproved:
		return "approved"
	case StateDeclined:
		return "declined"
	case StateExpired:
		return "expired"
	case StateCertRotated:
		return "cert_rotated"
	default:
		return "unknown"
	}
}

// Request is the in-memory record for a single pairing attempt.
// Fields are exported so the admin handler can render the snapshot
// returned by Store.List(); the live row inside the Store is held by
// pointer and never escapes Store methods directly.
type Request struct {
	ID               string
	DeviceName       string
	ClientVersion    string
	VerificationCode string
	// PollHash is the raw bytes of sha256(pollSecret), decoded once at
	// CreateRequest time so every Poll comparison runs against an
	// already-canonical [32]byte (no hex casing pitfall in the compare
	// path).
	PollHash [32]byte
	// CertFingerprint is the bridge's cert SHA-256 fingerprint at the
	// moment of request creation. Compared against the live fingerprint
	// at Approve time — mismatch transitions to CertRotated instead of
	// Approved, defending against a cert rotation between the iOS user
	// initiating Join and the admin clicking Approve.
	CertFingerprint string
	// SourceIP is the request's RemoteAddr host. Display-only — the
	// admin UI shows it so the operator can spot LAN spam patterns.
	SourceIP  string
	State     State
	TokenID   string // populated on Approve; tied to auth.Store.Mint return
	RawToken  string // populated on Approve; returned on every authorized poll until DELETE
	CreatedAt time.Time
	DecidedAt time.Time

	// expiryTimer is the per-request deadline. Reused across transitions:
	// Pending → fires at TTL → Expired; Approved → fires at TTL+grace
	// from creation → revoke + delete; terminal states → fires at grace
	// → delete. Always Stop()-ed before being replaced or row deletion.
	expiryTimer *time.Timer
}

// Options configures a Store. Zero values fall through to package defaults.
type Options struct {
	TTL        time.Duration
	Grace      time.Duration
	MaxPending int

	// Now is the clock source. nil defaults to time.Now. Tests inject a
	// controllable clock; production passes nil.
	Now func() time.Time

	// RevokeToken is invoked when an Approved request hits TTL+grace
	// without iOS acknowledging receipt via DELETE — defends against
	// orphaned tokens after a network blip kills the iOS poll between
	// mint and consume. nil means "never revoke" (test mode); production
	// wires this to auth.Store.Revoke.
	RevokeToken func(tokenID string) error
}

// Store holds the live set of pairing requests. Safe for concurrent use
// by HTTP handlers (CreateRequest / Poll / Delete) and the admin
// (List / Approve / Decline). Internally a single mutex serializes every
// state transition; the auth.Store mutex (taken by RevokeToken inside
// the sweeper) is never held while the pairing mutex is held — the
// timer callback releases the pairing mutex before invoking revoke.
type Store struct {
	mu   sync.Mutex
	byID map[string]*Request

	ttl        time.Duration
	grace      time.Duration
	maxPending int
	now        func() time.Time
	revoke     func(tokenID string) error
}

// NewStore constructs a Store with the given options.
func NewStore(opts Options) *Store {
	if opts.TTL <= 0 {
		opts.TTL = DefaultTTL
	}
	if opts.Grace <= 0 {
		opts.Grace = DefaultGrace
	}
	if opts.MaxPending <= 0 {
		opts.MaxPending = DefaultMaxPending
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Store{
		byID:       make(map[string]*Request),
		ttl:        opts.TTL,
		grace:      opts.Grace,
		maxPending: opts.MaxPending,
		now:        opts.Now,
		revoke:     opts.RevokeToken,
	}
}

// Sentinel errors returned by Store methods. Callers should branch via
// errors.Is for HTTP status mapping.
var (
	ErrNotFound       = errors.New("pairing: request not found")
	ErrUnauthorized   = errors.New("pairing: poll secret mismatch")
	ErrQueueFull      = errors.New("pairing: pending queue full")
	ErrAlreadyDecided = errors.New("pairing: request already decided")
	ErrCertRotated    = errors.New("pairing: cert fingerprint changed since request created")
	ErrBadHash        = errors.New("pairing: malformed pollSecretHash (must be 64 hex chars)")
)

// MintFunc is the Approve callback that creates the bearer token. The
// adapter at the call site wraps auth.Store.Mint(name) into this shape so
// the pairing package doesn't dictate the auth-store API.
type MintFunc func(name string) (rawToken, tokenID string, err error)

// CreateRequest stores a new Pending request and returns its snapshot
// (verification code + ID for the iOS POST response). Called from the
// /v1/pairing/requests handler.
func (s *Store) CreateRequest(deviceName, clientVersion, pollSecretHashHex, sourceIP, certFingerprint string) (Request, error) {
	if deviceName == "" {
		return Request{}, errors.New("pairing: deviceName must not be empty")
	}
	pollHash, err := decodeHashHex(pollSecretHashHex)
	if err != nil {
		return Request{}, err
	}
	id, err := randomHex(requestIDBytes)
	if err != nil {
		return Request{}, fmt.Errorf("random id: %w", err)
	}
	code, err := randomVerificationCode()
	if err != nil {
		return Request{}, fmt.Errorf("random code: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	pending := 0
	for _, r := range s.byID {
		if r.State == StatePending {
			pending++
		}
	}
	if pending >= s.maxPending {
		return Request{}, ErrQueueFull
	}

	// Avoid the (cosmically unlikely) ID collision so a fresh Create
	// can't replace a still-live request.
	for _, exists := s.byID[id]; exists; _, exists = s.byID[id] {
		id, err = randomHex(requestIDBytes)
		if err != nil {
			return Request{}, fmt.Errorf("random id (collision retry): %w", err)
		}
	}

	now := s.now()
	req := &Request{
		ID:               id,
		DeviceName:       deviceName,
		ClientVersion:    clientVersion,
		VerificationCode: code,
		PollHash:         pollHash,
		CertFingerprint:  certFingerprint,
		SourceIP:         sourceIP,
		State:            StatePending,
		CreatedAt:        now,
	}
	idCopy := id
	req.expiryTimer = time.AfterFunc(s.ttl, func() { s.onTimer(idCopy) })
	s.byID[id] = req

	return snapshot(req), nil
}

// PollResult is the wire shape Poll returns.
type PollResult struct {
	State               State
	TTLSecondsRemaining int
	Token               string // empty unless State == StateApproved
	TokenID             string // empty unless State == StateApproved
	VerificationCode    string // echoed so iOS can re-display after a foreground resume
}

// Poll authenticates the caller via pollSecret (raw, base64url-ish bytes)
// and returns the current state. The token is included on every
// authorized poll while the state is Approved, NOT read-once — see the
// "Token delivery contract" in the package doc.
func (s *Store) Poll(id, pollSecret string) (PollResult, error) {
	if pollSecret == "" {
		return PollResult{}, ErrUnauthorized
	}
	presented := sha256.Sum256([]byte(pollSecret))

	s.mu.Lock()
	defer s.mu.Unlock()

	req, ok := s.byID[id]
	if !ok {
		return PollResult{}, ErrNotFound
	}
	if subtle.ConstantTimeCompare(presented[:], req.PollHash[:]) != 1 {
		return PollResult{}, ErrUnauthorized
	}

	res := PollResult{
		State:            req.State,
		VerificationCode: req.VerificationCode,
	}
	if req.State == StatePending {
		deadline := req.CreatedAt.Add(s.ttl)
		rem := deadline.Sub(s.now())
		if rem < 0 {
			rem = 0
		}
		res.TTLSecondsRemaining = int(rem / time.Second)
	}
	if req.State == StateApproved {
		res.Token = req.RawToken
		res.TokenID = req.TokenID
	}
	return res, nil
}

// Delete removes a request after authenticating via pollSecret. Used by
// iOS for both user-cancel (Pending) and acknowledgment of token receipt
// (Approved). Idempotent at the HTTP layer — the handler maps ErrNotFound
// to 200 (already-deleted is success for an ack). The Store treats
// ErrNotFound as a real signal so callers can distinguish.
func (s *Store) Delete(id, pollSecret string) error {
	if pollSecret == "" {
		return ErrUnauthorized
	}
	presented := sha256.Sum256([]byte(pollSecret))

	s.mu.Lock()
	defer s.mu.Unlock()

	req, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	if subtle.ConstantTimeCompare(presented[:], req.PollHash[:]) != 1 {
		return ErrUnauthorized
	}
	if req.expiryTimer != nil {
		req.expiryTimer.Stop()
	}
	delete(s.byID, id)
	return nil
}

// List returns a snapshot of every request, for the admin Devices page.
// Sorted by CreatedAt ascending so the oldest pending shows first
// (admin tends to triage in arrival order).
func (s *Store) List() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, 0, len(s.byID))
	for _, req := range s.byID {
		out = append(out, snapshot(req))
	}
	// Sort newest-first is more useful for an at-a-glance triage; the
	// admin handler can re-sort if it disagrees.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.Before(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Approve transitions a Pending request to Approved, mints a bearer
// token via the injected MintFunc, and schedules the undelivered-revoke
// deadline. Returns the post-transition snapshot.
//
// `currentFingerprint` is the bridge's cert fingerprint at approve time.
// If the captured-at-create fingerprint differs, the request transitions
// to CertRotated (refusing to approve onto a new cert) and ErrCertRotated
// is returned. iOS sees a terminal cert_rotated state and prompts re-pair.
//
// MintFunc is called while Store.mu is held — callers must ensure mint
// can't deadlock by re-entering the pairing store. auth.Store.Mint
// satisfies this (separate mutex, no callbacks back into pairing).
func (s *Store) Approve(id, currentFingerprint string, mint MintFunc) (Request, error) {
	if mint == nil {
		return Request{}, errors.New("pairing: Approve requires a MintFunc")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	req, ok := s.byID[id]
	if !ok {
		return Request{}, ErrNotFound
	}
	if req.State != StatePending {
		return snapshot(req), ErrAlreadyDecided
	}
	// Cert-rotation guard. Empty captured fingerprint disables the
	// check (used by tests that don't wire a fingerprint); production
	// always supplies one.
	if req.CertFingerprint != "" && currentFingerprint != "" && req.CertFingerprint != currentFingerprint {
		req.State = StateCertRotated
		req.DecidedAt = s.now()
		s.scheduleTimer(req, s.grace)
		return snapshot(req), ErrCertRotated
	}

	rawToken, tokenID, err := mint(req.DeviceName)
	if err != nil {
		return Request{}, fmt.Errorf("mint: %w", err)
	}
	req.State = StateApproved
	req.DecidedAt = s.now()
	req.TokenID = tokenID
	req.RawToken = rawToken

	// Approved requests live until iOS DELETEs (acknowledgment) or the
	// undelivered-revoke deadline at CreatedAt + TTL + Grace. Compute
	// what's left from creation; floor at 1s so a request approved at
	// the very edge of TTL still gets a window for iOS to consume.
	elapsed := s.now().Sub(req.CreatedAt)
	remaining := s.ttl + s.grace - elapsed
	if remaining < time.Second {
		remaining = time.Second
	}
	s.scheduleTimer(req, remaining)
	return snapshot(req), nil
}

// Decline transitions a Pending request to Declined and schedules the
// grace-window cleanup. Returns the post-transition snapshot.
func (s *Store) Decline(id string) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	req, ok := s.byID[id]
	if !ok {
		return Request{}, ErrNotFound
	}
	if req.State != StatePending {
		return snapshot(req), ErrAlreadyDecided
	}
	req.State = StateDeclined
	req.DecidedAt = s.now()
	s.scheduleTimer(req, s.grace)
	return snapshot(req), nil
}

// TTLSeconds returns the configured Pending TTL as whole seconds. Used
// by the iOS-facing handler so the POST response advertises the same
// window the Pending sweeper enforces. Floors at 1 for non-zero TTLs so
// a sub-second TTL (only used in tests) still surfaces a positive value.
func (s *Store) TTLSeconds() int {
	n := int(s.ttl / time.Second)
	if n == 0 && s.ttl > 0 {
		return 1
	}
	return n
}

// Close stops every per-request timer. Call on clean shutdown so the
// timer goroutines drain promptly. Safe to call multiple times.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, req := range s.byID {
		if req.expiryTimer != nil {
			req.expiryTimer.Stop()
			req.expiryTimer = nil
		}
	}
}

// onTimer is invoked from time.AfterFunc when a request's current-phase
// deadline elapses. Branches on State:
//
//   - Pending  → transition to Expired, reschedule for grace cleanup.
//   - Approved → undelivered: revoke the minted token, delete the row.
//   - Declined / Expired / CertRotated → grace elapsed, delete the row.
//
// Revoke runs OUTSIDE the pairing mutex — auth.Store has its own mutex
// and we don't want a slow disk persist to block a concurrent CreateRequest.
func (s *Store) onTimer(id string) {
	s.mu.Lock()
	var revokeID string
	if req, ok := s.byID[id]; ok {
		switch req.State {
		case StatePending:
			req.State = StateExpired
			req.DecidedAt = s.now()
			s.scheduleTimer(req, s.grace)
		case StateApproved:
			revokeID = req.TokenID
			delete(s.byID, id)
		case StateDeclined, StateExpired, StateCertRotated:
			delete(s.byID, id)
		}
	}
	s.mu.Unlock()

	if revokeID != "" && s.revoke != nil {
		if err := s.revoke(revokeID); err != nil {
			logger.Error("revoke undelivered token", "id", id, "tokenID", revokeID, "err", err)
		}
	}
}

// scheduleTimer Stops the request's prior timer (if any) and arms a new
// one for d from now. Caller must hold s.mu.
func (s *Store) scheduleTimer(req *Request, d time.Duration) {
	if req.expiryTimer != nil {
		req.expiryTimer.Stop()
	}
	id := req.ID
	req.expiryTimer = time.AfterFunc(d, func() { s.onTimer(id) })
}

// snapshot returns a value copy of the request without the live timer
// pointer, so callers can't mutate Store internals through the returned
// Request.
func snapshot(req *Request) Request {
	cp := *req
	cp.expiryTimer = nil
	return cp
}

// decodeHashHex parses the hex form of a SHA-256 hash into [32]byte.
// Strict — wrong length or non-hex bytes both surface ErrBadHash.
func decodeHashHex(s string) ([32]byte, error) {
	var out [32]byte
	if len(s) != 64 {
		return out, ErrBadHash
	}
	n, err := hex.Decode(out[:], []byte(s))
	if err != nil || n != 32 {
		return out, ErrBadHash
	}
	return out, nil
}

// randomHex returns 2*nBytes hex chars from crypto/rand.
func randomHex(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// randomVerificationCode returns a 6-digit decimal code from crypto/rand,
// formatted with leading zeros so "004123" survives string round-trips
// (vs "4123" if stored as int and re-formatted).
//
// The mod-1_000_000 introduces a vanishingly small modulo bias (2^32 is
// not divisible by 10^6) — for a verification code the bias is
// inconsequential against the threat (admin reads it off the iPhone
// before approving; an attacker who can guess the code still has to
// pass the pollSecret bearer check on the poll endpoint).
func randomVerificationCode() (string, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	n := binary.BigEndian.Uint32(buf[:]) % 1_000_000
	return fmt.Sprintf("%06d", n), nil
}
