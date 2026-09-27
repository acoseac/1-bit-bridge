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
// advanced. Writes use atomic rename so a reader never sees a torn file, and
// re-read the file just before that rename so they never replace a sibling's
// write they were not built from (commitLocked).
package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging"
)

var logger = logging.Component("auth")

// beforeValidatePersistHook is a test-only seam (nil in production)
// fired inside Validate's debounced-persist branch, immediately
// before the pre-persist reloadIfStale. It lets a test drop a
// sibling-process write into the exact window the reload guards, to
// prove the debounced persist doesn't clobber a concurrent mint.
// Follows the afterExtractHookForTests convention in the manifest
// scanner: production cost is one nil-check per persist (at most once
// per lastUsedFlushInterval per token), negligible.
var beforeValidatePersistHook func()

// beforeCommitHook is a test-only seam (nil in production), fired in
// every write between the staging of the file and the re-read that gates
// its commit: the window a sibling process's write lands in. Same
// convention as beforeValidatePersistHook, and as adminauth's
// beforeCommitHook.
var beforeCommitHook func()

// errStoreMoved is a write finding the file changed between the read it
// was built from and its commit.
var errStoreMoved = errors.New("the token store changed while it was being written; nothing written")

// maxCommitAttempts bounds how often a write rebuilds for a file that
// keeps changing under it. Each attempt costs a staging and an fsync, and
// a file another process rewrites inside three of them in a row is not a
// `bridge pair` anyone is running by hand. A debounced write stays in
// memory for the next one; a Mint, Revoke, Rotate or SetExpiry fails.
const maxCommitAttempts = 3

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
// LastClientVersion records the most recent X-Client-Version header
// value this token presented (additive in protocol v1; absent on
// older clients). LastClientVersionAt is the "last *changed*"
// timestamp — when the value first transitioned to whatever
// LastClientVersion currently holds — NOT a per-request "last seen"
// marker. That keeps RecordClientVersion's hot path lock-free under
// steady-state traffic (same value, no field write needed). Use
// LastUsedAt for "when did this token last present credentials" and
// LastClientVersionAt for "when did its self-reported version most
// recently change".
//
// The auto-installer's compat gate (Phase C) reads LastClientVersion
// to decide whether a candidate update would orphan a still-active
// iOS build.
type Token struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	Hash                string    `json:"hash"` // SHA-256 hex of the raw token bytes
	CreatedAt           time.Time `json:"createdAt"`
	LastUsedAt          time.Time `json:"lastUsedAt,omitempty"`
	LastClientVersion   string    `json:"lastClientVersion,omitempty"`
	LastClientVersionAt time.Time `json:"lastClientVersionAt,omitempty"`

	// RotatedAt is set by Rotate when the raw bytes are replaced
	// (Hash gets a new value, ID/Name/CreatedAt stay). Zero means
	// the token has never been rotated. After rotation, the
	// "ID = first 12 hex chars of Hash" invariant from Mint no
	// longer holds for this row — that's a deliberate UX trade so
	// the operator's reference (admin URL, log line, runbook) stays
	// stable across a rotation. Mint still derives the ID from the
	// hash for new tokens.
	RotatedAt time.Time `json:"rotatedAt,omitempty"`

	// ExpiresAt is the optional hard cutoff. nil/absent means "never
	// expires" (the historical behaviour). Validate rejects with
	// ErrExpired once the wall-clock crosses ExpiresAt — ahead of
	// the constant-time hash compare so a leaked-but-expired raw
	// token can't be used. Stored as `*time.Time` to distinguish
	// "operator cleared the expiry" (nil) from "never set" (omitted)
	// across YAML/JSON round-trips.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
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

	mu     sync.Mutex
	tokens []Token
	// loaded + lastSize together identify the on-disk file state we
	// reflect in memory. mtime alone is insufficient: many filesystems
	// (FAT32, several NAS exports, some ZFS configurations) coarsen
	// mtime to 1 s, so a sibling process (`bridge pair` writing while
	// `bridge serve` is running) can land a write within the same tick
	// our last persist captured. In that scenario `info.ModTime() ==
	// s.loaded` and an mtime-only check skips the reload, so Validate
	// refuses the new token until the file changes again or this
	// process next writes (whose pre-commit re-read reloads it; before
	// that re-read existed, the write dropped the token). Using size
	// as a tiebreaker catches the realistic same-tick scenarios:
	//   - Mint (size grows by one token's JSON shape)
	//   - Revoke (size shrinks by one token's JSON shape)
	//   - First-ever Rotate (RotatedAt field toggles from omitted to
	//     RFC3339Nano-stamped)
	//   - Any insert/clear of a timestamp field that toggles between
	//     omitempty-absent and present.
	//
	// Known limitation (CodeRabbit on PR #159): re-rotating an
	// already-rotated token leaves Hash + RotatedAt both as same-
	// length strings (Hash is 64 hex chars; RFC3339Nano stays
	// constant-length once sub-second precision is established).
	// Two such rotations landing in the same mtime tick would
	// produce a same-size file and slip past this check.
	// Fingerprinting via SHA256 content hash was considered;
	// rejected because reloadIfStale runs on every authenticated
	// request and reading the file (or hashing it) on the hot path
	// is too costly. The narrowness of the residual race
	// (operator-initiated double-rotation, sub-1s apart, byte-
	// equal serialization) makes the trade favour the cheap check.
	// It bounds only what a READ sees: a write compares the file's
	// bytes (raw) before it commits, so it never replaces such a
	// rotation, and rebuilding around it reloads the file.
	loaded        time.Time
	lastSize      int64
	isEmpty       bool      // tokens file didn't exist when we last looked
	lastUsedFlush time.Time // last persist() driven by a LastUsedAt update

	// raw is the file's bytes as of the read s.tokens was last built
	// from (reload) or the write that last put it down (writeLocked);
	// nil for a file that was missing. A write commits only while the
	// file still holds exactly these bytes. Not a hot-path cost: it is
	// compared once per write, and writes are debounced.
	raw []byte

	// staticHash / staticTok hold the config-seeded demo token
	// (SetStaticToken), nil/zero unless one was installed. Held OUTSIDE
	// the JSON-backed token list on purpose: the static token must
	// survive a tokens.json wipe (that is its whole reason to exist)
	// and must never be persisted — the config file already owns it,
	// and writing it into tokens.json would let `bridge token revoke`
	// half-remove it (revoked on disk, resurrected from config at the
	// next boot), a confusing split-brain.
	staticHash []byte
	staticTok  Token
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

// SetStaticToken installs a single in-memory, config-seeded bearer token
// alongside the JSON-backed ones (demo mode's `demo.tokenSHA256`).
// sha256Hex is the hex SHA-256 of the raw token — the raw value never
// reaches this process — and name labels the synthetic device in logs.
// Malformed hex is rejected with an error so a typo'd config line fails
// loudly at boot instead of silently never matching. Calling it again
// replaces the previous static entry. Deliberately not persisted; see
// the staticHash field docblock for why.
func (s *Store) SetStaticToken(sha256Hex, name string) error {
	decoded, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(sha256Hex)))
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("static token: want %d hex chars (a SHA-256 digest), got %d", sha256.Size*2, len(sha256Hex))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.staticHash = decoded
	hexLower := hex.EncodeToString(decoded)
	s.staticTok = Token{
		// Same "first 12 hex chars of the hash" convention Mint uses,
		// so admin/log surfaces render a familiar-shaped ID.
		ID:        hexLower[:tokenIDLen],
		Name:      name,
		Hash:      hexLower,
		CreatedAt: time.Now().UTC(),
	}
	return nil
}

// reload refreshes the in-memory view from disk unconditionally. Caller must
// hold s.mu.
func (s *Store) reload() error {
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.tokens = nil
		s.isEmpty = true
		s.loaded = time.Time{}
		s.lastSize = 0
		s.raw = nil
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
	// Preserve in-memory LastUsedAt + LastClientVersion bumps that the
	// 30 s debounce in Validate / RecordClientVersion hasn't written
	// to disk yet. Without this, an out-of-process write (e.g. a
	// concurrent `bridge pair` appending a new token) fires
	// reloadIfStale, which overwrites our token slice with disk
	// contents whose timestamps predate the in-memory bumps — wiping
	// the debounce's in-flight work. The invariant is "in-memory
	// observation state never regresses across reload"; enforce it
	// here per token ID.
	if len(s.tokens) > 0 {
		type priorState struct {
			lastUsedAt          time.Time
			lastClientVersion   string
			lastClientVersionAt time.Time
		}
		prior := make(map[string]priorState, len(s.tokens))
		for _, old := range s.tokens {
			prior[old.ID] = priorState{
				lastUsedAt:          old.LastUsedAt,
				lastClientVersion:   old.LastClientVersion,
				lastClientVersionAt: old.LastClientVersionAt,
			}
		}
		for i := range tokens {
			p, ok := prior[tokens[i].ID]
			if !ok {
				continue
			}
			if p.lastUsedAt.After(tokens[i].LastUsedAt) {
				tokens[i].LastUsedAt = p.lastUsedAt
			}
			// Newer in-memory client-version observation wins over a
			// stale disk-side one. We use LastClientVersionAt as the
			// "is this fresher" marker because LastClientVersion is a
			// string (no temporal ordering of its own).
			if p.lastClientVersionAt.After(tokens[i].LastClientVersionAt) {
				tokens[i].LastClientVersion = p.lastClientVersion
				tokens[i].LastClientVersionAt = p.lastClientVersionAt
			}
		}
	}
	s.tokens = tokens
	s.isEmpty = false
	s.loaded = info.ModTime()
	s.lastSize = info.Size()
	s.raw = raw
	return nil
}

// reloadIfStale compares the current file mtime AND size to what we loaded
// and reloads if either differs. Called from Validate so a `bridge pair` run
// picks up automatically in a concurrently-running `bridge serve`. Caller
// must hold mu. See Store.lastSize for the rationale on the size tiebreaker.
func (s *Store) reloadIfStale() error {
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		if !s.isEmpty {
			s.tokens = nil
			s.isEmpty = true
			s.loaded = time.Time{}
			s.lastSize = 0
			s.raw = nil
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !info.ModTime().Equal(s.loaded) || info.Size() != s.lastSize {
		return s.reload()
	}
	return nil
}

// persist writes the in-memory token list as it stands: the write of the
// debounced LastUsedAt and client-version observations and of the
// shutdown flush, whose change is already in memory. Caller must hold mu.
func (s *Store) persist() error {
	return s.commitLocked(nil)
}

// commitLocked writes the token list build returns and adopts it into
// memory once the write has landed, so a write that fails leaves memory
// as it was. Every write of the store goes through here. Caller must hold
// mu, and has read the file (reload or reloadIfStale) first.
//
// build is handed the list as last read and returns the list to write, or
// an error that ends the write with nothing written (ErrNotFound for a
// token that is no longer there). It may run more than once and must not
// modify the list it is handed. A nil build writes the list as it stands.
//
// Staging the file costs a temp file, a write and an fsync (a median of
// 0.6 ms on ext4 and 2.3 to 3.8 ms on APFS, measured; tens on a cloud
// disk), in which a sibling process (`bridge pair`, `bridge token
// revoke`) can commit. Renaming over that commit dropped the token it
// minted or brought back the one it revoked. So the file is read once
// more just before the rename (unchangedSinceReadLocked), and a change
// starts the write again from a fresh read: reload, whose per-token merge
// keeps this process's unwritten LastUsedAt and client-version
// observations, then build again, up to maxCommitAttempts. A re-read or
// a reload that fails ends the write with nothing written, as a failed
// reload before the first staging does. What remains is the rename
// itself, and on Windows its retries: see adminauth's commitLocked, which
// closes the same window the same way.
func (s *Store) commitLocked(build func(cur []Token) ([]Token, error)) error {
	for attempt := 1; ; attempt++ {
		next := s.tokens
		if build != nil {
			var err error
			if next, err = build(s.tokens); err != nil {
				return err
			}
		}
		err := s.writeLocked(next)
		if err == nil {
			s.tokens = next
			return nil
		}
		if !errors.Is(err, errStoreMoved) || attempt == maxCommitAttempts {
			return err
		}
		if err := s.reload(); err != nil {
			return fmt.Errorf("re-read the token store, which changed while it was being written; nothing written: %w", err)
		}
	}
}

// writeLocked stages tokens beside the store and renames them over it,
// provided the file is still the one they were built from
// (unchangedSinceReadLocked); otherwise it answers errStoreMoved, or the
// re-read's error, having written nothing. Caller must hold mu.
func (s *Store) writeLocked(tokens []Token) error {
	// 0o700 on the parent dir matches the 0o600 file mode — keeps the
	// whole token store inaccessible on multi-user hosts.
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("mkdir token store: %w", err)
	}
	data, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// os.CreateTemp creates with mode 0o600 modulo umask — and umask
	// only *removes* permission bits, so the resulting file is at most
	// 0o600. The explicit Chmod here is belt-and-braces against unusual
	// filesystems whose ACLs widen perms on close (some network mounts)
	// and against future Go behaviour drift on the temp-file mode. Cost
	// is one syscall; correctness benefit is structural.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".tokens-*.json")
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
	// Panic-safety net: every error path below explicitly Close()s
	// before returning, but a panic between CreateTemp and the
	// explicit Close (e.g. inside an http.Handler that net/http will
	// recover from) would otherwise leak the file descriptor. The
	// explicit Close on the success path runs first; this defer's
	// second Close returns fs.ErrClosed and is ignored. Ordering is
	// load-bearing — registered AFTER the Remove defer so it runs
	// FIRST (LIFO), freeing the FD before Remove tries to unlink
	// (Windows holds an open file from being removed).
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
	// What this write records as the file it put down is the staged
	// file's own mtime and size, which the rename leaves as they are,
	// never a stat of the path afterwards: RenameWithRetry fsyncs the
	// directory after renaming (a median of 0.5 ms on ext4 and 2.8 ms on
	// APFS, measured), and a sibling's commit landing in that fsync was
	// recorded as this process's own file, so reloadIfStale saw nothing
	// new and Validate refused a device paired there until this process
	// next wrote. Taken after the Close: Windows may settle a file's last
	// write time only when its last writing handle closes.
	staged, err := os.Stat(tmpName)
	if err != nil {
		return fmt.Errorf("stat tmp: %w", err)
	}
	if err := s.unchangedSinceReadLocked(); err != nil {
		return err
	}
	if err := atomicwrite.RenameWithRetry(tmpName, s.path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	tmpName = "" // suppress defer cleanup
	s.raw = data
	s.loaded = staged.ModTime()
	s.lastSize = staged.Size()
	s.isEmpty = false
	// Every successful write resets the LastUsedAt debounce clock —
	// whether the write was driven by Validate, Mint, Revoke, or
	// FlushLastUsed — so callers don't have to remember to stamp it
	// themselves and Mint/Revoke also get the debounce benefit for free.
	s.lastUsedFlush = time.Now()
	return nil
}

// unchangedSinceReadLocked is the check a write makes between its staging
// and its rename. It answers errStoreMoved when the file no longer holds,
// byte for byte, what s.raw records (a missing file reads as nil, as in
// reload), and an error when the file cannot be read: a file this process
// cannot see may hold a token it has never seen, so neither commits.
// Split out of writeLocked for SonarCloud's go:S3776 (cognitive
// complexity), as adminauth's unchangedSince is. Caller must hold mu.
func (s *Store) unchangedSinceReadLocked() error {
	if beforeCommitHook != nil {
		beforeCommitHook()
	}
	latest, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		latest, err = nil, nil
	}
	if err != nil {
		return fmt.Errorf("re-read the token store before the commit; nothing written: %w", err)
	}
	if !bytes.Equal(latest, s.raw) {
		return errStoreMoved
	}
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
	if err := s.commitLocked(func(cur []Token) ([]Token, error) {
		next := make([]Token, 0, len(cur)+1)
		next = append(next, cur...)
		return append(next, tok), nil
	}); err != nil {
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
	now := time.Now()
	for i := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(s.tokens[i].Hash), []byte(hashHex)) == 1 {
			// Expiry check sits AFTER the hash compare (so the
			// timing remains constant against the token list, no
			// short-circuit revealing "this hash matches but is
			// expired") but BEFORE LastUsedAt is bumped (so an
			// expired token's last-used stamp doesn't tick on
			// every poll). An expired token validates as a miss.
			if s.tokens[i].ExpiresAt != nil && !s.tokens[i].ExpiresAt.IsZero() && now.After(*s.tokens[i].ExpiresAt) {
				return Token{}, false
			}
			// The token struct wants a wall-clock UTC value so the JSON
			// round-trip is readable; the debounce gate uses `time.Since`
			// which reads the monotonic clock and so survives NTP jumps.
			s.tokens[i].LastUsedAt = now.UTC()
			// Capture the matched token BEFORE any reload below. The
			// pre-persist reloadIfStale can swap s.tokens for a fresh
			// slice (different length/order after a sibling write), so
			// s.tokens[i] would no longer name this match — return the
			// captured copy instead. Value-identical to s.tokens[i] on
			// the no-reload path.
			matched := s.tokens[i]
			if time.Since(s.lastUsedFlush) >= lastUsedFlushInterval {
				if beforeValidatePersistHook != nil {
					beforeValidatePersistHook()
				}
				// Cross-process safety: a sibling `bridge pair` /
				// `bridge revoke` may have rewritten tokens.json since
				// the top-of-method reloadIfStale ran — s.mu is
				// process-local and does NOT serialize another PROCESS's
				// write. This reload takes such a write in before the
				// staging. One that lands DURING the staging is
				// commitLocked's: it re-reads the file before its rename
				// and rebuilds around a change, and it would also catch
				// this one, after a staging wasted on the stale list.
				// reload's per-token merge preserves the LastUsedAt bump
				// above, so neither read can lose it. Mirrors
				// RecordClientVersion.
				//
				// A FAILED reload must ABORT the persist, on FlushLastUsed's
				// rationale: dropping a debounced timestamp is recoverable
				// (the bump stays in memory — reload leaves s.tokens
				// untouched on every error path — and lands at the next
				// successful flush, or at shutdown); an overwrite that
				// deletes a sibling's freshly-minted token is not. The
				// realistic trigger is Windows, where a sibling
				// `bridge pair` replacing tokens.json can hand this
				// process's ReadFile an ERROR_SHARING_VIOLATION inside the
				// AV / indexer scan-on-close window — the exact window
				// atomicwrite.RenameWithRetry exists for. Ignoring the
				// error there, before commitLocked re-read the file,
				// erased the just-paired device's token and 401'd it from
				// the next reload on.
				if err := s.reloadIfStale(); err != nil {
					logger.Error("reload before persisting LastUsedAt; skipping persist to avoid clobbering a sibling write", "err", err)
				} else if err := s.persist(); err != nil {
					logger.Error("persist LastUsedAt", "err", err)
				}
				// persist() stamps `lastUsedFlush` on success; nothing to
				// do here on any branch. The validation verdict below is
				// unaffected either way — it is already decided.
			}
			return matched, true
		}
	}
	// Config-seeded static token (demo mode). Checked AFTER the
	// JSON-backed list so the existing timing shape over the file
	// tokens is untouched; the compare itself is constant-time. No
	// LastUsedAt bump and no persist — the static entry lives only in
	// memory and is re-seeded from config at every boot.
	if s.staticHash != nil && subtle.ConstantTimeCompare(hashBytes[:], s.staticHash) == 1 {
		return s.staticTok, true
	}
	return Token{}, false
}

// FlushLastUsed forces a persist of any in-memory LastUsedAt updates
// that the debounce in Validate has not yet written. Call on clean
// shutdown so a just-before-exit validate doesn't lose its timestamp.
// persist() itself updates `lastUsedFlush`, so nothing else to do here.
//
// Cross-process safety: a sibling `bridge pair` / `bridge revoke` may
// have rewritten tokens.json since this process last loaded it. If no
// authenticated request followed (Validate is what triggers the routine
// reloadIfStale), the in-memory slice is stale, and writing it at
// shutdown silently deleted the freshly-minted token (or resurrected a
// revoked one). The reload takes that write in before the staging;
// commitLocked's re-read before the rename covers one landing during
// it. reload's per-token merge preserves the in-memory LastUsedAt /
// LastClientVersion bumps this flush exists to land, so the reload can't
// lose them. A reload failure aborts the flush — dropping a debounced
// timestamp is recoverable; an overwrite that deletes a sibling's token
// is not.
func (s *Store) FlushLastUsed() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadIfStale(); err != nil {
		return fmt.Errorf("reload before flush: %w", err)
	}
	return s.persist()
}

// RecordClientVersion stores the iOS app version a client identified
// itself as via the X-Client-Version request header. Called from the
// authed() middleware on every authenticated request whose
// X-Client-Version is non-empty AND differs from the value the
// middleware's token-copy already shows (the cheap pre-check happens
// in api.authed; this method always re-checks under the mutex).
//
// Persistence honours the same 30-second `lastUsedFlush` debounce as
// LastUsedAt updates. Without that gate, a misbehaving or malicious
// client could rotate its X-Client-Version on every request and force
// synchronous tokens.json rewrites under the global lock — a DoS
// vector against every other authenticated request. Bounded to one
// persist per 30 s, the in-memory state still tracks the latest
// value (so the updater's compat gate sees fresh data) and the
// shutdown FlushLastUsed call lands any deferred update on disk.
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
		// Byte-slicing can land in the middle of a multi-byte UTF-8
		// rune. Trim back to the last valid boundary so we never
		// persist a half-rune to tokens.json — encoding/json would
		// substitute a replacement character on the next read but
		// the on-disk shape is still malformed.
		//
		// TrimPartialTrailingRune drops at most UTFMax-1 trailing bytes
		// and is O(1). The older `for !utf8.ValidString(ver)` loop
		// rescanned the whole string per iteration AND, on an input
		// with INTERIOR invalid UTF-8, walked back past the bad byte
		// discarding everything after it. That shape is only safe for
		// inputs guaranteed valid except at the cut — which a header
		// value is, so this was never a live bug here; it's one less
		// copy of a pattern the codebase has deprecated.
		ver = fsutil.TrimPartialTrailingRune(ver[:maxClientVersionLen])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.tokens {
		if s.tokens[i].ID != id {
			continue
		}
		// Common case: same version, no need to touch fields or disk.
		// (api.authed already does this check against its token-copy
		// to avoid the lock entirely; we re-check under the mutex
		// because that copy may have been stale.)
		if s.tokens[i].LastClientVersion == ver {
			return
		}
		s.tokens[i].LastClientVersion = ver
		s.tokens[i].LastClientVersionAt = time.Now().UTC()
		// Same 30-second debounce as LastUsedAt — see method-level
		// doc. FlushLastUsed on shutdown lands any deferred update.
		if time.Since(s.lastUsedFlush) >= lastUsedFlushInterval {
			// Cross-process safety: a concurrent `bridge pair` /
			// `bridge revoke` may have written tokens.json since
			// the in-memory snapshot was last loaded. Writing our
			// slice back would resurrect a revoked token or drop a
			// freshly-paired one: this reloadIfStale takes such a
			// write in before the staging, and commitLocked's
			// re-read before the rename covers one that lands
			// during it. The reload's per-token merge (above)
			// preserves our in-memory LastClientVersion bump, so a
			// successful reload still ends up writing the new value.
			//
			// A FAILED reload ABORTS the persist — same contract as
			// Validate and FlushLastUsed. The in-memory bump above
			// survives (reload mutates nothing on its error paths) and
			// lands at the next successful flush or at shutdown; writing
			// the stale slice over a sibling's file would not be
			// recoverable.
			if err := s.reloadIfStale(); err != nil {
				logger.Error("reload before persisting client-version; skipping persist to avoid clobbering a sibling write", "err", err)
			} else if err := s.persist(); err != nil {
				logger.Error("persist client-version", "err", err)
			}
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

// Get returns the token matching id, or ErrNotFound if none exists.
// The returned struct is a copy — mutating it has no effect on the
// store. Used by the admin token-lifecycle handlers as a cheap
// single-row lookup vs. the O(N) `List()`-then-scan pattern Gemini
// flagged on PR #45 review.
func (s *Store) Get(id string) (Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reloadIfStale()
	for i := range s.tokens {
		if s.tokens[i].ID == id {
			return s.tokens[i], nil
		}
	}
	return Token{}, ErrNotFound
}

// Revoke removes the token with the given ID. Returns ErrNotFound if no such
// token exists.
func (s *Store) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return err
	}
	return s.commitLocked(func(cur []Token) ([]Token, error) {
		i := indexOfToken(cur, id)
		if i < 0 {
			return nil, ErrNotFound
		}
		// A FRESH backing array: an in-place `append(cur[:i],
		// cur[i+1:]...)` would shift the list memory still holds, and a
		// write that fails must leave memory as it was (reloadIfStale
		// won't resync — the failed write didn't change the file's mtime
		// or size — so a still-valid token would be rejected until
		// restart). Adopting `next` on success also releases the removed
		// Token (+ its ExpiresAt pointer) for GC.
		next := make([]Token, 0, len(cur)-1)
		next = append(next, cur[:i]...)
		return append(next, cur[i+1:]...), nil
	})
}

// indexOfToken returns the index of the token with id in tokens, or -1.
func indexOfToken(tokens []Token, id string) int {
	for i := range tokens {
		if tokens[i].ID == id {
			return i
		}
	}
	return -1
}

// Rotate replaces the raw bytes of an existing token, returning the
// new raw token for re-pairing. The token's ID, Name, CreatedAt, and
// ExpiresAt are preserved across rotation; only Hash and RotatedAt
// change. The previous raw token stops validating immediately.
//
// This is the operator path for "this token was leaked / I want a
// fresh secret without losing the row identity". Pairs with iOS's
// existing "scan a fresh QR" re-pair flow — the operator hands the
// new raw to the device-holder, who scans it from the admin
// console's pair URL or types it into the Bridge Editor.
//
// Returns ErrNotFound if no token with that ID exists.
func (s *Store) Rotate(id string) (rawToken string, tok Token, err error) {
	var buf [rawTokenBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", Token{}, fmt.Errorf("random: %w", err)
	}
	rawToken = base64.RawURLEncoding.EncodeToString(buf[:])
	hashBytes := sha256.Sum256([]byte(rawToken))
	hashHex := hex.EncodeToString(hashBytes[:])

	rotatedAt := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return "", Token{}, err
	}
	err = s.commitLocked(func(cur []Token) ([]Token, error) {
		i := indexOfToken(cur, id)
		if i < 0 {
			return nil, ErrNotFound
		}
		next := slices.Clone(cur)
		next[i].Hash = hashHex
		next[i].RotatedAt = rotatedAt
		tok = next[i]
		return next, nil
	})
	if err != nil {
		return "", Token{}, err
	}
	return rawToken, tok, nil
}

// SetExpiry installs (or clears) the ExpiresAt field for an
// existing token. Pass nil to remove an existing expiry. Returns
// ErrNotFound if no token with that ID exists.
//
// Validation is permissive about backwards-set expiries — passing
// a past timestamp immediately invalidates the token (operator
// "expire this now" path). The CLI surfaces a `--in <duration>`
// flag that resolves to `time.Now().Add(d)`; admin UI can pass
// any wall-clock RFC3339.
func (s *Store) SetExpiry(id string, expiresAt *time.Time) (Token, error) {
	var set *time.Time
	if expiresAt != nil {
		utc := expiresAt.UTC()
		set = &utc
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return Token{}, err
	}
	var tok Token
	if err := s.commitLocked(func(cur []Token) ([]Token, error) {
		i := indexOfToken(cur, id)
		if i < 0 {
			return nil, ErrNotFound
		}
		next := slices.Clone(cur)
		next[i].ExpiresAt = set
		tok = next[i]
		return next, nil
	}); err != nil {
		return Token{}, err
	}
	return tok, nil
}

// ErrNotFound is returned by Revoke / Rotate / SetExpiry when the
// given ID is unknown.
var ErrNotFound = errors.New("token not found")
