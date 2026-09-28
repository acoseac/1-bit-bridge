package adminauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// LoginTicketTTL is the DEFAULT lifetime of a one-time console login ticket.
//
// Deliberately short. A ticket travels in a URL, so it can land in shell
// history, a browser's address bar and its history database; the defence is that
// it is single-use and stale quickly, not that those places are private.
//
// 60 seconds is calibrated for the flow this was WRITTEN for: `bridge admin
// login-link` typed into a shell and pasted into a browser on the same machine.
// A cross-device hand-off is a different budget, and it was failing at 60s: the
// hosted control plane mints while the user is holding a PHONE, the URL then
// travels by AirDrop or a message, and a human has to notice it on the other
// machine and click. The mint itself is a sudo + `as_tenant` + bridge-binary
// round trip before the share sheet even appears, so the clock is already
// running. `bridge-tenant console-link` therefore passes its own TTL through
// MintLoginTicketTTL; the default stays where it is correct.
const LoginTicketTTL = 60 * time.Second

// MaxLoginTicketTTL bounds what a caller may ask for.
//
// A ceiling rather than a free parameter, because what is being extended is how
// long a live credential sitting in a URL stays live. Ten minutes covers a
// cross-device hand-off with room for an unlock-the-laptop detour, and is still
// far short of a window worth harvesting a browser history for.
const MaxLoginTicketTTL = 10 * time.Minute

// maxLiveTickets bounds the live ticket files a mint adds to. Tickets are
// minted by an operator (or by a control plane acting for one), never by an
// anonymous request, so this is a sanity ceiling rather than an anti-abuse
// control. A mint at the ceiling is REFUSED and evicts nothing: every file is
// a link somebody may be holding. The count is taken per mint, so two
// processes minting at the same moment can each pass it by one.
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
// Tickets are PERSISTED, each in a file of its own beside the store.
//
// An earlier version kept them in memory, reasoning that a ticket outliving a
// restart was a credential written to disk for no benefit. That was wrong, and
// only a live test found it: `bridge admin login-link` runs as a SEPARATE
// PROCESS from the serving bridge, so a ticket minted in the CLI's memory died
// with the CLI and the server redeemed nothing. Every unit test passed, because
// each minted and redeemed inside one process.
//
// What is stored is the SHA-256, never the ticket: it names the ticket's file,
// which holds the account and the expiry, 0600, in the directory that already
// holds the password hashes. So the disk gains no credential it did not
// already hold, and single use and the lifetime come from the file being
// removed on presentation.
//
// One file per ticket, because two processes handle them and Store.mu reaches
// neither from the other: `bridge admin login-link` mints and the serving
// bridge redeems. Until 2026-09-28 every live ticket shared one file, which
// each process rewrote whole from its own earlier read. A mint whose read
// predated a redemption renamed the spent ticket back into place, live again
// for the rest of its lifetime (up to MaxLoginTicketTTL), and a redemption
// whose read predated a mint dropped the new ticket. Re-reading the file just
// before the rename, as the store file's commitLocked does, narrows that and
// cannot close it: on Windows the rename itself retries for up to 750 ms
// (atomicwrite.RenameWithRetry). Now no process rewrites a ticket it did not
// mint. A mint creates one file, a redemption removes one, and each is atomic
// on its own.
func (s *Store) MintLoginTicket(username string) (string, error) {
	return s.MintLoginTicketTTL(username, 0)
}

// MintLoginTicketTTL is MintLoginTicket with an explicit lifetime.
//
// A ttl of zero or less means LoginTicketTTL, so a caller with no opinion gets
// the shell-local default. A ttl above MaxLoginTicketTTL is REFUSED rather than
// silently clamped: the caller asked for something specific, and quietly
// shortening it would leave an operator believing a link lives longer than it
// does — which is this whole parameter's own bug, inverted.
func (s *Store) MintLoginTicketTTL(username string, ttl time.Duration) (string, error) {
	if username == "" {
		return "", errors.New("login ticket needs a username")
	}
	if ttl <= 0 {
		ttl = LoginTicketTTL
	}
	if ttl > MaxLoginTicketTTL {
		return "", fmt.Errorf("login ticket ttl %s exceeds the %s maximum", ttl, MaxLoginTicketTTL)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The account is the FILE's (the Store docblock's first rule), so read
	// it rather than trust what was there when this process opened.
	if err := s.refreshCredentialLocked(); err != nil {
		return "", err
	}
	if s.user == nil || s.user.Username != username {
		// Refuse to mint for an account that does not exist, so a ticket can
		// never authenticate as someone the store has never heard of.
		return "", fmt.Errorf("no such admin user %q", username)
	}
	now := s.clock()
	live, err := s.pruneTicketsLocked(now)
	if err != nil {
		return "", err
	}
	if live >= maxLiveTickets {
		return "", errors.New("too many live login tickets")
	}
	buf := make([]byte, 32)
	// No error check: since Go 1.24 crypto/rand.Read never returns one — it
	// crashes the program irrecoverably rather than handing back short or
	// predictable bytes. A branch here would be unreachable, and would suggest
	// to a reader that a weaker fallback exists somewhere.
	rand.Read(buf)
	raw := base64.RawURLEncoding.EncodeToString(buf)
	if err := s.writeTicketFileLocked(hashTicket(raw), persistedTicket{
		Username:  username,
		ExpiresAt: now.Add(ttl).UnixNano(),
	}); err != nil {
		return "", err
	}
	// The file every live ticket shared until 2026-09-28. Nothing reads it
	// now, and what it held expired within MaxLoginTicketTTL of the upgrade,
	// so the first mint after it removes it. Best effort: a leftover holds
	// nothing that can redeem, and the next mint tries again.
	_ = os.Remove(s.legacyTicketPath())
	return raw, nil
}

// RedeemLoginTicket consumes a ticket and returns the account it authenticates.
// A ticket is spent whether or not it turned out to be valid for this caller, so
// a redemption can never be retried.
//
// It reads and removes that ticket's own file and nothing else, so it can
// neither drop a ticket minted meanwhile nor bring back one spent meanwhile.
// A ticket that does not exist costs one open of a name that is not there,
// and writes nothing. That is the branch an unauthenticated POST reaches,
// under the mutex every authenticated console request takes, and it used to
// read the whole shared file and rewrite it whenever a record in it had
// expired (the 2026-09-09 LOUPE measured an unconditional rewrite at 3.93 ms a
// request).
//
// ErrTicketInvalid is an answer about the TICKET: not there, expired, spent,
// or for an account the store no longer has. Any other error is an answer
// about the STORE — the ticket's file or the credential could not be read,
// or the ticket could not be removed — and leaves the ticket unspent, so the
// same link works once the fault is fixed. The admin handler answers the
// first with the stale-link page and the second with a 500.
func (s *Store) RedeemLoginTicket(raw string) (string, error) {
	if raw == "" {
		return "", ErrTicketInvalid
	}
	path := s.ticketFilePath(hashTicket(raw))
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	// Read from disk every time: the process that minted this is usually not
	// the process redeeming it.
	t, err := readTicketFile(path)
	switch {
	case errors.Is(err, errTicketDamaged):
		// Every ticket file is renamed into place complete, so one that does
		// not parse was never a mint's (a hand edit, a damaged disk). It
		// redeems nothing; remove it so it stops being asked about.
		if rmErr := os.Remove(path); rmErr != nil && !ticketAbsent(rmErr, runtime.GOOS) {
			return "", fmt.Errorf("remove a damaged login ticket: %w", rmErr)
		}
		return "", ErrTicketInvalid
	case err != nil && ticketAbsent(err, runtime.GOOS):
		return "", ErrTicketInvalid
	case err != nil:
		// The record may be there and live. Nothing is established about it,
		// so this is a store fault, never ErrTicketInvalid, whose stale-link
		// page would send the holder for a fresh link while this one waits.
		return "", fmt.Errorf("read login ticket: %w", err)
	}
	// The account check below reads the credential from the FILE: the process
	// that replaces an account is never the one redeeming, and this one's copy
	// is the account as it was at start. Read here, on the hit path only, so a
	// bogus ticket never reads the credential file, and before the ticket is
	// spent, so a credential this process cannot read establishes nothing
	// about the ticket and leaves it on disk, as a ticket file that cannot be
	// removed does.
	if err := s.refreshCredentialLocked(); err != nil {
		return "", err
	}
	if beforeTicketSpendHook != nil {
		beforeTicketSpendHook()
	}
	// Remove before judging: a ticket presented once is used up either way, so
	// a caller cannot probe one repeatedly while waiting for a clock edge. The
	// removal is also what makes the spend single: of two redemptions that
	// both read the file, exactly one removes it.
	if err := os.Remove(path); err != nil {
		if ticketAbsent(err, runtime.GOOS) {
			// Removed since the read: spent by another redemption, or pruned
			// as expired by a mint in another process.
			return "", ErrTicketInvalid
		}
		// A Windows sharing violation, a directory this process may read and
		// not write: the ticket is still there, and pressing Continue again
		// can redeem it.
		return "", fmt.Errorf("spend login ticket: %w", err)
	}
	if !ticketLive(t, now) {
		return "", ErrTicketInvalid
	}
	// Re-assert the account under the lock we already hold, against the file
	// read above. MintLoginTicket checks this, but the two happen in
	// different PROCESSES with up to MaxLoginTicketTTL between them, and
	// CreateSession validates nothing — so an account replaced inside the
	// window would otherwise mint a fully-privileged session for a username
	// the store no longer has.
	if s.user == nil || s.user.Username != t.Username {
		return "", ErrTicketInvalid
	}
	return t.Username, nil
}

// persistedTicket is one ticket's file. The file's NAME carries the hex
// SHA-256 of the ticket (ticketFilePath), so neither the name nor the file is
// a usable credential.
type persistedTicket struct {
	Username  string `json:"username"`
	ExpiresAt int64  `json:"expiresAt"`
}

func hashTicket(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ticketLive reports whether a ticket can still redeem at now. The one
// predicate for a mint's prune and a redemption's judgement, so the two
// cannot disagree about a ticket at its last instant.
func ticketLive(t persistedTicket, now time.Time) bool {
	return now.Before(time.Unix(0, t.ExpiresAt))
}

// ticketFileSuffix ends every ticket file's name.
const ticketFileSuffix = ".json"

// storeBase is the store file's name without its extension: "adminauth" for
// adminauth.json. The ticket files are named from it, so two stores in one
// directory keep their tickets apart.
func (s *Store) storeBase() string {
	return strings.TrimSuffix(filepath.Base(s.path), filepath.Ext(s.path))
}

// ticketFilePrefix opens every ticket file's name: "adminauth-ticket-".
func (s *Store) ticketFilePrefix() string {
	return s.storeBase() + "-ticket-"
}

// ticketFilePath is the file of the ticket whose SHA-256 is digest (hex), in
// the store's own directory: adminauth-ticket-<digest>.json.
func (s *Store) ticketFilePath(digest string) string {
	return filepath.Join(filepath.Dir(s.path), s.ticketFilePrefix()+digest+ticketFileSuffix)
}

// ticketDigestFromName returns the digest a directory entry's name carries,
// and whether the name is a ticket file of this store at all: the prefix, 64
// lowercase hex characters, the suffix, and nothing else. Anything else is
// left alone by the prune, which removes only what a mint could have written.
// A staging file is dot-prefixed and never matches.
func (s *Store) ticketDigestFromName(name string) (string, bool) {
	prefix := s.ticketFilePrefix()
	if len(name) != len(prefix)+hex.EncodedLen(sha256.Size)+len(ticketFileSuffix) ||
		!strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ticketFileSuffix) {
		return "", false
	}
	digest := name[len(prefix) : len(name)-len(ticketFileSuffix)]
	for i := 0; i < len(digest); i++ {
		c := digest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return digest, true
}

// legacyTicketPath is the one file every live ticket shared until 2026-09-28,
// adminauth-tickets.json. Nothing reads it; the first mint removes it.
func (s *Store) legacyTicketPath() string {
	return filepath.Join(filepath.Dir(s.path), s.storeBase()+"-tickets.json")
}

// errTicketDamaged is a ticket file that was read and does not parse.
var errTicketDamaged = errors.New("login ticket file is damaged")

// readTicketFile reads one ticket's file. A read error comes back as it is,
// for ticketAbsent to sort; a parse error wraps errTicketDamaged.
func readTicketFile(path string) (persistedTicket, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return persistedTicket{}, err
	}
	var t persistedTicket
	if err := json.Unmarshal(b, &t); err != nil {
		return persistedTicket{}, fmt.Errorf("%w: %s: %v", errTicketDamaged, path, err)
	}
	return t, nil
}

// ticketAbsent reports whether err, from reading or removing a ticket's file,
// means the file is not there: the ticket was spent, pruned, or never minted.
// goos is runtime.GOOS at every call site. It is a parameter so the Windows
// arm runs on every CI leg, where a check that only runs on one looks exactly
// like one that passed.
//
// On Windows a permission error reads as absent too. A file whose delete is
// pending because another handle holds it open (a mint's prune racing an
// antivirus scanner's handle, the window atomicwrite.RenameWithRetry exists
// for) stays in the directory until that handle closes, and DeleteFile's
// documentation says every CreateFile of it meanwhile fails with
// ERROR_ACCESS_DENIED, which Go reports as fs.ErrPermission. Only a spent or an
// expired ticket is ever removed, so such a file was on its way out, and
// answering 500 for it would call a used-up link a broken store. The cost: on
// Windows a genuine ACL fault on one ticket file reads as a stale link, where
// POSIX answers 500. Neither answer authenticates anyone.
func ticketAbsent(err error, goos string) bool {
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	return goos == "windows" && errors.Is(err, fs.ErrPermission)
}

// pruneTicketsLocked removes the ticket files that can never redeem again and
// counts the ones that still can. Caller MUST hold s.mu.
//
// Expired and damaged files are removed, best effort: one that stays is
// removed by the next mint, and a redemption of it answers ErrTicketInvalid
// either way. A file this process cannot read is neither removed nor
// ignored: it may be live, so it holds a place under maxLiveTickets. A file
// gone between the listing and its read was spent meanwhile. Live files are
// only read, so a mint leaves every ticket but its own byte for byte as it
// found it.
//
// An entry that is not a regular file is not a ticket, whatever its name:
// every ticket is a regular file a mint renamed into place, and nothing else
// the bridge writes here takes that name. Such an entry is skipped, neither
// counted nor removed. A directory, or a link to one, fails its read with an
// error that is not absence, so it was counted as possibly live, and
// maxLiveTickets of them refused every mint. The type comes from the listing
// (an lstat where the filesystem does not report one), so a symlink is
// skipped whatever it points to.
//
// Pruning happens here and nowhere else. Its old home was a redemption's miss
// branch, which an unauthenticated request reaches; a mint is an operator's.
func (s *Store) pruneTicketsLocked(now time.Time) (int, error) {
	entries, err := os.ReadDir(filepath.Dir(s.path))
	if err != nil {
		return 0, fmt.Errorf("list login tickets: %w", err)
	}
	live := 0
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		digest, ok := s.ticketDigestFromName(e.Name())
		if !ok {
			continue
		}
		path := s.ticketFilePath(digest)
		t, err := readTicketFile(path)
		switch {
		case err == nil && ticketLive(t, now):
			live++
		case err == nil, errors.Is(err, errTicketDamaged):
			_ = os.Remove(path)
		case ticketAbsent(err, runtime.GOOS):
			// Spent, or pruned by another mint, since the listing.
		default:
			live++
		}
	}
	return live, nil
}

// writeTicketFileLocked puts one ticket's file in place and returns once it
// has landed. Caller MUST hold s.mu. The file is new (its name is a fresh
// ticket's digest), so nothing another process wrote is replaced.
func (s *Store) writeTicketFileLocked(digest string, t persistedTicket) error {
	body, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode login ticket: %w", err)
	}
	path := s.ticketFilePath(digest)
	// A UNIQUE staging name from os.CreateTemp, as every writer in this
	// package uses: it creates at 0600 modulo umask, and umask can only
	// REMOVE bits, so the Chmod below is belt-and-braces against filesystems
	// that widen on close — the convention auth.Store already follows.
	// filepath.Dir, not the dir half of filepath.Split: Split returns "" for
	// a path with no separator, and os.CreateTemp("") stages in os.TempDir()
	// — a different filesystem on a normal Linux host, where the rename then
	// fails EXDEV. Dir returns "." instead, which is what auth.Store and this
	// package's own store.go already do. Production always passes an absolute
	// path, so this is hardening, not a live fix.
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("stage login ticket: %w", err)
	}
	tmpName := tmp.Name()
	// The two-defer idiom, matching writeStoreLocked in this package and
	// auth.Store: LIFO runs Close BEFORE Remove, which is what Windows needs
	// (it will not unlink an open file), and it also closes the descriptor if
	// anything between here and the rename panics. The explicit Close calls on
	// the error paths below stay — a double Close returns an error nobody
	// reads, and they make each path's intent legible on its own line.
	// (Gemini, PR #880.)
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	defer func() { _ = tmp.Close() }()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod login ticket: %w", err)
	}
	// `sudo bridge admin login-link` stages this file as root, and the
	// serving bridge must still read it to redeem the ticket, and remove it.
	// The file is new, so it takes the owner of the directory it lands in.
	if err := fsutil.KeepOwner(tmp, path); err != nil {
		tmp.Close()
		return fmt.Errorf("keep the login ticket's owner: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write login ticket: %w", err)
	}
	// Sync before the rename, like every sibling persist site in the tree
	// (adminauth/store.go, auth/auth.go, config/config.go).
	// RenameWithRetry fsyncs the directory ENTRY; nothing else flushes the
	// CONTENTS, so a crash could publish a durable entry to a file whose
	// blocks were never written. Fail-safe either way (zeroed bytes do not
	// parse, and a damaged ticket redeems nothing), but "each site keeps its
	// own Chmod / Sync / parent-dir fsync" is the rule.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync login ticket: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close login ticket: %w", err)
	}
	if beforeTicketCommitHook != nil {
		beforeTicketCommitHook()
	}
	if err := atomicwrite.RenameWithRetry(tmpName, path); err != nil {
		return fmt.Errorf("commit login ticket: %w", err)
	}
	tmpName = "" // renamed away; the defer must not remove the committed file
	return nil
}

// beforeTicketCommitHook is a test-only seam (nil in production), fired in a
// mint between the staging of its ticket's file and the rename that puts it
// in place: the window a shared file lost another process's write in. Same
// convention as beforeCommitHook.
var beforeTicketCommitHook func()

// beforeTicketSpendHook is a test-only seam (nil in production), fired in a
// redemption between the read of its ticket's file and the removal that
// spends it: the window in which a mint's ticket was dropped when every ticket
// shared one file. Same convention as beforeCommitHook.
var beforeTicketSpendHook func()

// clock reads the injectable clock. Callers hold s.mu.
func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}
