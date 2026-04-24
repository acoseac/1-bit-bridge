package auth

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"
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
