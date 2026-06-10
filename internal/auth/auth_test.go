package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func newTmpStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return s, path
}

// loadedForTest exposes the in-memory mtime snapshot for cross-process
// race tests. Method lives on *Store but in a _test.go file, so it
// compiles only during test runs and never ships in the binary.
func (s *Store) loadedForTest() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loaded
}

func TestOpenStoreMissingFile(t *testing.T) {
	s, path := newTmpStore(t)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected missing file, got %v", err)
	}
	if got := s.List(); len(got) != 0 {
		t.Errorf("empty store has tokens: %v", got)
	}
}

func TestMintProducesValidRawAndRecord(t *testing.T) {
	s, _ := newTmpStore(t)
	raw, tok, err := s.Mint("iPhone 15 Pro")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Raw token is 43-char base64url of 32 random bytes.
	if len(raw) != 43 {
		t.Errorf("raw token length = %d, want 43", len(raw))
	}
	if _, err := base64.RawURLEncoding.DecodeString(raw); err != nil {
		t.Errorf("raw token is not base64url: %v", err)
	}

	if tok.Name != "iPhone 15 Pro" {
		t.Errorf("tok.Name = %q", tok.Name)
	}
	if len(tok.Hash) != 64 {
		t.Errorf("tok.Hash length = %d, want 64", len(tok.Hash))
	}
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(tok.ID) {
		t.Errorf("tok.ID = %q", tok.ID)
	}
	if tok.CreatedAt.IsZero() {
		t.Error("tok.CreatedAt is zero")
	}
}

func TestMintRejectsEmptyName(t *testing.T) {
	s, _ := newTmpStore(t)
	if _, _, err := s.Mint(""); err == nil {
		t.Error("expected error for empty name")
	}
}

func TestValidateHitsNewlyMinted(t *testing.T) {
	s, _ := newTmpStore(t)
	raw, tok, _ := s.Mint("iPad")
	got, ok := s.Validate(raw)
	if !ok {
		t.Fatal("Validate returned false for a freshly minted token")
	}
	if got.ID != tok.ID {
		t.Errorf("ID = %q, want %q", got.ID, tok.ID)
	}
}

func TestValidateMissesUnknownToken(t *testing.T) {
	s, _ := newTmpStore(t)
	s.Mint("iPad")
	_, ok := s.Validate("not-a-real-token")
	if ok {
		t.Error("Validate returned true for unknown token")
	}
}

func TestValidateEmptyStringAlwaysMisses(t *testing.T) {
	s, _ := newTmpStore(t)
	s.Mint("device")
	if _, ok := s.Validate(""); ok {
		t.Error("empty token unexpectedly validated")
	}
}

func TestValidateUpdatesLastUsedAt(t *testing.T) {
	s, _ := newTmpStore(t)
	raw, _, _ := s.Mint("Mac")
	t0 := time.Now().UTC()
	time.Sleep(5 * time.Millisecond)
	if _, ok := s.Validate(raw); !ok {
		t.Fatal("Validate miss")
	}
	tokens := s.List()
	if len(tokens) != 1 {
		t.Fatalf("List len = %d", len(tokens))
	}
	if !tokens[0].LastUsedAt.After(t0) {
		t.Errorf("LastUsedAt not updated: %v (expected > %v)", tokens[0].LastUsedAt, t0)
	}
}

func TestValidateDebouncesLastUsedPersist(t *testing.T) {
	// Rapid Validate hits within the debounce window must NOT rewrite
	// tokens.json — the window is lastUsedFlushInterval. A subsequent
	// FlushLastUsed persists the pending timestamp on clean shutdown.
	s, path := newTmpStore(t)
	raw, _, _ := s.Mint("Mac")

	// Mint already stamped `lastUsedFlush` via its own persist(), so a
	// subsequent Validate lands inside the debounce window and must NOT
	// re-persist. (The old behaviour stamped `lastUsedFlush` only from
	// within Validate's success branch; centralising the stamp in
	// persist() means Mint/Revoke paths also debounce subsequent hits,
	// which is the invariant this assertion pins.)
	if _, ok := s.Validate(raw); !ok {
		t.Fatal("first validate miss")
	}
	mtAfterFirst := mustMtime(t, path)

	// Rapid follow-up validates must NOT re-persist.
	for i := 0; i < 5; i++ {
		time.Sleep(2 * time.Millisecond)
		if _, ok := s.Validate(raw); !ok {
			t.Fatal("follow-up validate miss")
		}
	}
	if mt := mustMtime(t, path); mt.After(mtAfterFirst) {
		t.Errorf("debounce broken: rapid validates rewrote tokens.json (%v → %v)", mtAfterFirst, mt)
	}

	// FlushLastUsed on shutdown must persist pending updates even
	// though the debounce hasn't elapsed.
	time.Sleep(5 * time.Millisecond)
	if err := s.FlushLastUsed(); err != nil {
		t.Fatalf("FlushLastUsed: %v", err)
	}
	if mt := mustMtime(t, path); !mt.After(mtAfterFirst) {
		t.Errorf("FlushLastUsed did not persist: %v (want > %v)", mt, mtAfterFirst)
	}
}

func mustMtime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

func TestPersistenceRoundTrip(t *testing.T) {
	s, path := newTmpStore(t)
	raw, tok, _ := s.Mint("iPhone")

	// Reopen from disk and verify the same raw token still validates.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	tokens := s2.List()
	if len(tokens) != 1 || tokens[0].ID != tok.ID {
		t.Errorf("reloaded tokens = %v", tokens)
	}
	if got, ok := s2.Validate(raw); !ok || got.ID != tok.ID {
		t.Errorf("reloaded validate failed: ok=%v id=%q", ok, got.ID)
	}
}

func TestMultipleTokensCoexist(t *testing.T) {
	s, _ := newTmpStore(t)
	rawA, _, _ := s.Mint("iPhone")
	rawB, _, _ := s.Mint("iPad")
	if rawA == rawB {
		t.Error("Mint produced identical raw tokens")
	}
	if _, ok := s.Validate(rawA); !ok {
		t.Error("A should validate")
	}
	if _, ok := s.Validate(rawB); !ok {
		t.Error("B should validate")
	}
	if len(s.List()) != 2 {
		t.Errorf("List = %d tokens, want 2", len(s.List()))
	}
}

func TestRevokeRemoves(t *testing.T) {
	s, _ := newTmpStore(t)
	raw, tok, _ := s.Mint("iPhone")
	if err := s.Revoke(tok.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, ok := s.Validate(raw); ok {
		t.Error("revoked token still validates")
	}
	if len(s.List()) != 0 {
		t.Errorf("List not empty after revoke")
	}
}

func TestRevokeUnknownReturnsErrNotFound(t *testing.T) {
	s, _ := newTmpStore(t)
	err := s.Revoke("deadbeefcafe")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Revoke(unknown) = %v, want ErrNotFound", err)
	}
}

func TestReloadPreservesNewerInMemoryLastUsed(t *testing.T) {
	// Regression guard: an out-of-process write to tokens.json (e.g.
	// `bridge pair` appending a new token) must not clobber in-memory
	// LastUsedAt bumps that the 30 s debounce in Validate has not yet
	// persisted. Under the pre-fix reload() the in-memory slice was
	// replaced wholesale with disk contents, wiping the pending bump —
	// exactly the regression PR #16's Gemini review flagged.
	s, path := newTmpStore(t)
	raw, tok, _ := s.Mint("iPhone")

	// Bump LastUsedAt in memory via Validate. The debounce stamped by
	// Mint's persist() means this Validate does NOT re-persist, so the
	// bump is strictly in-memory.
	time.Sleep(5 * time.Millisecond)
	if _, ok := s.Validate(raw); !ok {
		t.Fatal("validate miss")
	}
	inMemory := s.List()[0].LastUsedAt
	if inMemory.IsZero() {
		t.Fatal("in-memory LastUsedAt was never bumped")
	}

	// Simulate an external writer (bridge pair) rewriting tokens.json
	// with an older LastUsedAt — zero-valued, since the external writer
	// doesn't see the in-memory bump. We reuse the raw file path and a
	// fresh store to generate a consistent on-disk payload, then bump
	// mtime so reloadIfStale triggers.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = s2.Mint("external") // persists both tokens, no LastUsedAt on iPhone
	// Force the mtime forward so s's next Validate triggers reloadIfStale.
	newMtime := time.Now().Add(time.Second)
	if err := os.Chtimes(path, newMtime, newMtime); err != nil {
		t.Fatal(err)
	}

	// List() on the original store — this calls reloadIfStale, which
	// overwrites s.tokens with the disk contents. Crucially, List does
	// NOT bump LastUsedAt (unlike Validate), so whatever we observe here
	// is strictly the merge's output. The merge must preserve the newer
	// in-memory LastUsedAt for the iPhone token: a post-reload assertion
	// that runs through Validate first would be indistinguishable from a
	// broken merge, because Validate stamps LastUsedAt = time.Now() and
	// the non-zero/>=inMemory invariants would pass trivially.
	var iPhoneAfterReload time.Time
	for _, tt := range s.List() {
		if tt.ID == tok.ID {
			iPhoneAfterReload = tt.LastUsedAt
			break
		}
	}
	if iPhoneAfterReload.IsZero() {
		t.Fatalf("iPhone token LastUsedAt reset to zero after reload — merge clobbered in-memory bump: %v", s.List())
	}
	if iPhoneAfterReload.Before(inMemory) {
		t.Errorf("reload regressed LastUsedAt: %v < %v", iPhoneAfterReload, inMemory)
	}

	// Belt-and-braces: Validate still succeeds post-reload. This is now
	// pure follow-on coverage — the merge has already been verified above
	// independently of any Validate-side bump.
	if _, ok := s.Validate(raw); !ok {
		t.Fatal("post-reload validate miss")
	}
}

func TestPickUpExternalMint(t *testing.T) {
	// Simulate `bridge pair` writing to the same file while `bridge serve`
	// (represented by s1) is running. s1 must pick up the new token on its
	// next Validate without being told to reload explicitly.
	s1, path := newTmpStore(t)
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, _ := s2.Mint("external-client")

	// Bump mtime deterministically in case the OS coarse-grained the first
	// write close enough to s1's load time to tie.
	newMtime := time.Now().Add(time.Second)
	if err := os.Chtimes(path, newMtime, newMtime); err != nil {
		t.Fatal(err)
	}

	if _, ok := s1.Validate(raw); !ok {
		t.Error("s1 did not pick up the externally-minted token")
	}
}

func TestPickUpExternalMintSameMtime(t *testing.T) {
	// Coarse filesystem mtime resolution (1 s on FAT32 / many NAS exports)
	// can land a sibling-process write in the same tick as our last
	// persist. Pre-fix, reloadIfStale's `info.ModTime().After(s.loaded)`
	// returned false for that case and silently skipped the reload —
	// dropping the new token on the next persist. Post-fix, the size
	// tiebreaker catches it.
	//
	// Setup mirrors the production race:
	//   1. s1 (the long-running serve process) mints a token, persisting
	//      a real file with a real mtime. Without this seed step the
	//      tokens.json file doesn't yet exist when s1 captures `loaded`,
	//      and the resulting zero-time clamp tests the wrong invariant
	//      (Caught by CodeRabbit on PR #159's first commit.)
	//   2. s2 (the sibling `bridge pair` process) opens the same file
	//      and mints a second token, growing the file by one token's
	//      worth of bytes.
	//   3. We force-clamp the file's mtime back to s1's pre-step-2
	//      snapshot — deterministic on any host regardless of what the
	//      host filesystem actually reports as mtime granularity.
	//   4. s1.Validate(raw_from_s2) MUST hit. Pre-fix this fails: mtime
	//      Equal → reload skipped → s1 still has only its own token →
	//      Validate misses. Post-fix the size tiebreaker triggers reload.
	s1, path := newTmpStore(t)
	if _, _, err := s1.Mint("seed"); err != nil {
		t.Fatalf("s1 seed Mint: %v", err)
	}
	loaded := s1.loadedForTest()
	if loaded.IsZero() {
		t.Fatal("s1.loaded was zero after Mint — store not persisted as expected")
	}

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, _ := s2.Mint("external-client")

	if err := os.Chtimes(path, loaded, loaded); err != nil {
		t.Fatal(err)
	}
	// Verify the host filesystem honored the requested timestamp at
	// nanosecond precision. macOS APFS preserves nanos; some other
	// filesystems (FAT32, ext3) round to seconds. If the clamp didn't
	// take, the test would falsely pass via the !Equal(mtime) branch
	// instead of exercising the size tiebreaker — we'd be measuring
	// the wrong contract. Skip rather than fail so the test stays
	// useful on filesystems with looser mtime semantics (CodeRabbit
	// minor review on PR #159's second round).
	postChtimes, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !postChtimes.ModTime().Equal(loaded) {
		t.Skipf("host filesystem rounded the requested mtime (%v → %v); test cannot exercise size-tiebreaker path here",
			loaded, postChtimes.ModTime())
	}

	if _, ok := s1.Validate(raw); !ok {
		t.Error("s1 missed the externally-minted token under same-mtime write")
	}
}

func TestFlushLastUsedPreservesExternalMint(t *testing.T) {
	// Shutdown calls FlushLastUsed to land any debounced LastUsedAt
	// updates. If a sibling `bridge pair` process minted a token since
	// this process last loaded tokens.json and NO authenticated request
	// followed (Validate is what routinely triggers reloadIfStale), the
	// pre-fix persist rewrote the file from the stale in-memory slice
	// and silently deleted the fresh token. FlushLastUsed must reload
	// before persisting. The s2 Mint grows the file, so the size
	// tiebreaker makes the staleness check deterministic even under
	// coarse filesystem mtime.
	s1, path := newTmpStore(t)
	if _, _, err := s1.Mint("serve-process"); err != nil {
		t.Fatalf("s1 Mint: %v", err)
	}

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.Mint("external-pair"); err != nil {
		t.Fatalf("s2 Mint: %v", err)
	}

	if err := s1.FlushLastUsed(); err != nil {
		t.Fatalf("FlushLastUsed: %v", err)
	}

	// Re-read the file through a fresh store — the assertion is about
	// what FlushLastUsed left ON DISK, not s1's in-memory view.
	s3, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tok := range s3.List() {
		got[tok.Name] = true
	}
	if !got["external-pair"] {
		t.Error("FlushLastUsed deleted the externally-minted token from tokens.json")
	}
	if !got["serve-process"] {
		t.Error("FlushLastUsed lost s1's own token")
	}
}

func TestAtomicPersistNoPartialState(t *testing.T) {
	// If persist fails (e.g. dir is read-only), the in-memory tokens list
	// must be rolled back so the store stays consistent with disk.
	s, path := newTmpStore(t)
	dir := filepath.Dir(path)
	// Read-only the dir so CreateTemp/Rename fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip("cannot chmod temp dir for this test")
	}
	defer os.Chmod(dir, 0o755)

	_, _, err := s.Mint("should-fail")
	if err == nil {
		t.Skip("persist succeeded despite read-only dir — platform-dependent")
	}
	if len(s.List()) != 0 {
		t.Errorf("failed Mint left tokens in memory: %v", s.List())
	}
}

func TestConcurrentMints(t *testing.T) {
	s, _ := newTmpStore(t)
	const n = 20
	raws := make([]string, n)
	var wg sync.WaitGroup
	var mu sync.Mutex
	errs := 0

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			raw, _, err := s.Mint("device")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs++
				return
			}
			raws[idx] = raw
		}(i)
	}
	wg.Wait()

	if errs != 0 {
		t.Errorf("%d Mint calls errored", errs)
	}
	if len(s.List()) != n {
		t.Errorf("List = %d, want %d (data race on persist?)", len(s.List()), n)
	}
	for _, raw := range raws {
		if _, ok := s.Validate(raw); !ok {
			t.Errorf("post-race: token %s…%s did not validate", raw[:6], raw[len(raw)-6:])
		}
	}
}

func TestRecordClientVersionUpdatesInMemoryAndFlushPersists(t *testing.T) {
	// RecordClientVersion honours the same 30 s lastUsedFlush debounce
	// as LastUsedAt, so a call inside the debounce window touches
	// in-memory state but does NOT rewrite tokens.json. Asserting the
	// in-memory bump + the FlushLastUsed-driven persist together is
	// what pins the post-PR-#41-review behaviour: fast in-memory,
	// debounced disk, no DoS surface from version flip-flopping.
	s, path := newTmpStore(t)
	_, tok, _ := s.Mint("iPhone 15")
	mtBefore := mustMtime(t, path)

	time.Sleep(10 * time.Millisecond)
	s.RecordClientVersion(tok.ID, "1.2.3")
	// In-memory must be live for the updater's compat gate.
	if got := s.List()[0]; got.LastClientVersion != "1.2.3" {
		t.Errorf("LastClientVersion = %q, want 1.2.3 (in-memory)", got.LastClientVersion)
	}
	// Debounced: no fresh disk write within the 30 s window after Mint.
	if mt := mustMtime(t, path); mt.After(mtBefore) {
		t.Errorf("RecordClientVersion within debounce window persisted (mtime %v > %v)", mt, mtBefore)
	}
	// FlushLastUsed forces the deferred update to land — same path the
	// shutdown defer in cmd/bridge/main.go uses.
	if err := s.FlushLastUsed(); err != nil {
		t.Fatalf("FlushLastUsed: %v", err)
	}
	if mt := mustMtime(t, path); !mt.After(mtBefore) {
		t.Errorf("FlushLastUsed did not persist deferred client-version update (mtime %v == %v)", mt, mtBefore)
	}
}

func TestRecordClientVersionDebouncesUnderRapidChanges(t *testing.T) {
	// DoS-protection regression guard (PR #41 review): a buggy or
	// malicious client could rotate X-Client-Version on every request
	// and force tokens.json rewrites under the global lock. The
	// debounce caps that at one persist per lastUsedFlushInterval.
	s, path := newTmpStore(t)
	_, tok, _ := s.Mint("iPhone 15")
	mtAfterMint := mustMtime(t, path)

	for i := 0; i < 50; i++ {
		s.RecordClientVersion(tok.ID, fmt.Sprintf("1.%d.%d", i/10, i%10))
		time.Sleep(time.Millisecond)
	}
	if mt := mustMtime(t, path); mt.After(mtAfterMint) {
		t.Errorf("flood of distinct versions broke the debounce (mtime %v > %v)", mt, mtAfterMint)
	}
}

func TestRecordClientVersionSkipsDiskOnRepeat(t *testing.T) {
	// Hot-path coverage: same value, request after request, no
	// in-memory or on-disk churn.
	s, path := newTmpStore(t)
	_, tok, _ := s.Mint("iPhone 15")
	s.RecordClientVersion(tok.ID, "1.2.3")
	mtAfterFirst := mustMtime(t, path)

	for i := 0; i < 5; i++ {
		time.Sleep(2 * time.Millisecond)
		s.RecordClientVersion(tok.ID, "1.2.3") // same value, should no-op
	}
	if mt := mustMtime(t, path); mt.After(mtAfterFirst) {
		t.Errorf("repeat RecordClientVersion(same value) re-persisted (mtime %v > %v)", mt, mtAfterFirst)
	}
}

func TestRecordClientVersionIgnoresEmptyAndUnknown(t *testing.T) {
	s, _ := newTmpStore(t)
	_, tok, _ := s.Mint("iPhone")
	// Empty version string: no-op.
	s.RecordClientVersion(tok.ID, "")
	if v := s.List()[0].LastClientVersion; v != "" {
		t.Errorf("LastClientVersion = %q, want empty after no-op call", v)
	}
	// Empty ID: no-op (no panic).
	s.RecordClientVersion("", "1.2.3")
	// Unknown ID: silently skipped.
	s.RecordClientVersion("deadbeefcafe", "1.2.3")
	if v := s.List()[0].LastClientVersion; v != "" {
		t.Errorf("RecordClientVersion(unknown id) wrote to wrong token: got %q", v)
	}
}

func TestRecordClientVersionTruncatesOverlongInput(t *testing.T) {
	s, _ := newTmpStore(t)
	_, tok, _ := s.Mint("iPhone")
	junk := strings.Repeat("X", 500)
	s.RecordClientVersion(tok.ID, junk)
	got := s.List()[0].LastClientVersion
	if len(got) != maxClientVersionLen {
		t.Errorf("LastClientVersion length = %d, want %d (clamped)", len(got), maxClientVersionLen)
	}
}

func TestConstantTimeCompareDoesNotLeakLength(t *testing.T) {
	// Smoke test: Validate must reject a token whose hex hash prefix matches
	// but whose body differs. subtle.ConstantTimeCompare handles this via
	// bitwise-or-accumulate; test is a sanity check on the path.
	s, _ := newTmpStore(t)
	raw, _, _ := s.Mint("a")
	suffix := ""
	for i := 0; i < len(raw)-10; i++ {
		suffix += "X"
	}
	badSamePrefix := raw[:10] + suffix
	if len(badSamePrefix) != len(raw) {
		t.Fatalf("test setup: lengths differ (%d vs %d)", len(badSamePrefix), len(raw))
	}
	if _, ok := s.Validate(badSamePrefix); ok {
		t.Error("token with shared prefix but different suffix unexpectedly validated")
	}
}

// Rotate: the new raw token must validate; the old one must not.
// ID/Name/CreatedAt are preserved; Hash and RotatedAt change.
func TestRotateRotatesRawAndPreservesIdentity(t *testing.T) {
	s, _ := newTmpStore(t)
	rawOld, tok, err := s.Mint("iPhone")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Sanity: old raw validates initially.
	if _, ok := s.Validate(rawOld); !ok {
		t.Fatalf("pre-rotate: old raw must validate")
	}

	rawNew, rotated, err := s.Rotate(tok.ID)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if rawNew == rawOld {
		t.Errorf("rotation produced the same raw token — random source broken?")
	}
	if rotated.ID != tok.ID {
		t.Errorf("rotation changed ID (%q → %q); ID must stay stable", tok.ID, rotated.ID)
	}
	if rotated.Name != tok.Name {
		t.Errorf("rotation changed Name (%q → %q)", tok.Name, rotated.Name)
	}
	if !rotated.CreatedAt.Equal(tok.CreatedAt) {
		t.Errorf("rotation changed CreatedAt (%v → %v)", tok.CreatedAt, rotated.CreatedAt)
	}
	if rotated.RotatedAt.IsZero() {
		t.Errorf("rotation must stamp RotatedAt")
	}
	if rotated.Hash == tok.Hash {
		t.Errorf("rotation did not change Hash")
	}

	// Old raw must now fail.
	if _, ok := s.Validate(rawOld); ok {
		t.Errorf("post-rotate: old raw must NOT validate")
	}
	// New raw must succeed.
	if _, ok := s.Validate(rawNew); !ok {
		t.Errorf("post-rotate: new raw must validate")
	}
}

func TestRotateUnknownIDReturnsErrNotFound(t *testing.T) {
	s, _ := newTmpStore(t)
	if _, _, err := s.Rotate("ffffffffffff"); err != ErrNotFound {
		t.Errorf("Rotate(unknown): err = %v, want ErrNotFound", err)
	}
}

func TestSetExpiryFutureLeavesTokenValid(t *testing.T) {
	s, _ := newTmpStore(t)
	raw, tok, _ := s.Mint("iPhone")
	exp := time.Now().Add(1 * time.Hour)
	if _, err := s.SetExpiry(tok.ID, &exp); err != nil {
		t.Fatalf("SetExpiry: %v", err)
	}
	if _, ok := s.Validate(raw); !ok {
		t.Errorf("future-expiry token must still validate")
	}
}

func TestSetExpiryPastInvalidatesImmediately(t *testing.T) {
	s, _ := newTmpStore(t)
	raw, tok, _ := s.Mint("iPhone")
	exp := time.Now().Add(-1 * time.Hour)
	if _, err := s.SetExpiry(tok.ID, &exp); err != nil {
		t.Fatalf("SetExpiry: %v", err)
	}
	if _, ok := s.Validate(raw); ok {
		t.Errorf("past-expiry token must NOT validate")
	}
}

func TestSetExpiryNilClearsExpiry(t *testing.T) {
	s, _ := newTmpStore(t)
	raw, tok, _ := s.Mint("iPhone")
	exp := time.Now().Add(-1 * time.Hour)
	_, _ = s.SetExpiry(tok.ID, &exp)
	// Confirm expired state
	if _, ok := s.Validate(raw); ok {
		t.Fatalf("setup: expired token should not validate")
	}
	// Clear expiry — token should validate again.
	if _, err := s.SetExpiry(tok.ID, nil); err != nil {
		t.Fatalf("SetExpiry(nil): %v", err)
	}
	if _, ok := s.Validate(raw); !ok {
		t.Errorf("after clearing expiry, token must validate again")
	}
}

func TestSetExpiryUnknownIDReturnsErrNotFound(t *testing.T) {
	s, _ := newTmpStore(t)
	exp := time.Now().Add(1 * time.Hour)
	if _, err := s.SetExpiry("ffffffffffff", &exp); err != ErrNotFound {
		t.Errorf("SetExpiry(unknown): err = %v, want ErrNotFound", err)
	}
}

func TestGetReturnsCopy(t *testing.T) {
	s, _ := newTmpStore(t)
	_, tok, _ := s.Mint("iPhone")
	got, err := s.Get(tok.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != tok.ID || got.Name != tok.Name {
		t.Errorf("Get returned wrong row: %+v vs %+v", got, tok)
	}
	// Mutating the returned struct must not affect the store.
	got.Name = "MUTATED"
	again, _ := s.Get(tok.ID)
	if again.Name == "MUTATED" {
		t.Errorf("Get must return a copy; mutation leaked into store")
	}
}

func TestGetUnknownReturnsErrNotFound(t *testing.T) {
	s, _ := newTmpStore(t)
	if _, err := s.Get("ffffffffffff"); err != ErrNotFound {
		t.Errorf("Get(unknown): err = %v, want ErrNotFound", err)
	}
}

// TestRecordClientVersion_TruncatesAtUTF8Boundary pins the contract
// added by PR #N: when the X-Client-Version header exceeds
// `maxClientVersionLen` and the byte at that index lands mid-rune,
// we trim back to the last valid UTF-8 boundary so we never persist
// a half-rune to tokens.json. Pre-fix, a 65-byte string with a
// multi-byte rune at byte 64 was sliced to 64 bytes producing
// malformed UTF-8.
func TestRecordClientVersion_TruncatesAtUTF8Boundary(t *testing.T) {
	s, _ := newTmpStore(t)
	_, tok, err := s.Mint("test")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Build a 65-byte string ending in a 3-byte rune ('好' = E5 A5 BD).
	// First 62 bytes are ASCII, then the 3-byte rune fills bytes
	// 62..64. The byte-slice at maxClientVersionLen=64 lands mid-
	// rune — pre-fix that produced "...好"-with-truncated-tail
	// (byte 64 is the second byte of '好', byte 65 is dropped).
	long := strings.Repeat("a", 62) + "好"
	if len(long) != 65 {
		t.Fatalf("setup error: built len(long)=%d, want 65", len(long))
	}
	if !strings.HasSuffix(long, "好") {
		t.Fatalf("setup error: long doesn't end in 好")
	}

	s.RecordClientVersion(tok.ID, long)

	got, err := s.Get(tok.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// The stored version must be valid UTF-8.
	if !utf8.ValidString(got.LastClientVersion) {
		t.Errorf("stored version is not valid UTF-8: %q (bytes: % x)",
			got.LastClientVersion, []byte(got.LastClientVersion))
	}
	// And it must be the prefix that ends BEFORE the offending
	// rune (62 'a's, no 好) — the trim back to a valid boundary
	// drops the partial rune entirely.
	want := strings.Repeat("a", 62)
	if got.LastClientVersion != want {
		t.Errorf("trim landed wrong:\n  got  %q (len=%d)\n  want %q (len=%d)",
			got.LastClientVersion, len(got.LastClientVersion),
			want, len(want))
	}
}

// TestRecordClientVersion_ASCIIBoundaryUnchanged is the partner test:
// when the byte at `maxClientVersionLen` is at an ASCII boundary,
// truncation behaves exactly like the pre-fix byte-slice (drops to
// 64 bytes, no extra trim). Guards against the UTF-8-safe path
// over-truncating a perfectly valid input.
func TestRecordClientVersion_ASCIIBoundaryUnchanged(t *testing.T) {
	s, _ := newTmpStore(t)
	_, tok, err := s.Mint("test")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	long := strings.Repeat("a", 100) // pure ASCII, length 100
	s.RecordClientVersion(tok.ID, long)

	got, _ := s.Get(tok.ID)
	want := strings.Repeat("a", 64)
	if got.LastClientVersion != want {
		t.Errorf("ASCII-boundary trim landed wrong:\n  got  %q (len=%d)\n  want %q (len=%d)",
			got.LastClientVersion, len(got.LastClientVersion),
			want, len(want))
	}
}
