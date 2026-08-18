package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
)

func staticHashFor(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// SetStaticToken → Validate round-trip: the raw token authenticates and
// carries the configured name plus the Mint-shaped 12-char ID.
func TestStaticTokenValidates(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	const raw = "demo-raw-token-fixture"
	if err := s.SetStaticToken(staticHashFor(raw), "Demo access (config)"); err != nil {
		t.Fatal(err)
	}
	tok, ok := s.Validate(raw)
	if !ok {
		t.Fatal("static token should validate")
	}
	if tok.Name != "Demo access (config)" {
		t.Errorf("name = %q", tok.Name)
	}
	if len(tok.ID) != tokenIDLen || !strings.HasPrefix(staticHashFor(raw), tok.ID) {
		t.Errorf("ID = %q, want first %d hex chars of the hash", tok.ID, tokenIDLen)
	}
	if _, ok := s.Validate("not-the-token"); ok {
		t.Error("wrong raw token should not validate")
	}
}

// Uppercase hex is normalized; malformed digests are rejected loudly
// (the boot-time contract — a typo'd config line must not silently
// never match).
func TestStaticTokenHexShapes(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	const raw = "demo-raw-token-fixture"
	if err := s.SetStaticToken(strings.ToUpper(staticHashFor(raw)), "Demo"); err != nil {
		t.Fatalf("uppercase hex should be accepted, got %v", err)
	}
	if _, ok := s.Validate(raw); !ok {
		t.Error("uppercase-seeded static token should validate")
	}
	for _, bad := range []string{"", "abc", strings.Repeat("g", 64), staticHashFor(raw)[:63]} {
		if err := s.SetStaticToken(bad, "Demo"); err == nil {
			t.Errorf("SetStaticToken(%q) should error", bad)
		}
	}
}

// The static entry coexists with minted tokens and survives the
// persist/reload cycle a Mint triggers — the wipe-resilience property
// it exists for (tokens.json can vanish; the config re-seeds).
func TestStaticTokenCoexistsWithMintedAndSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	const raw = "demo-raw-token-fixture"
	if err := s.SetStaticToken(staticHashFor(raw), "Demo"); err != nil {
		t.Fatal(err)
	}
	minted, _, err := s.Mint("paired sibling")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Validate(minted); !ok {
		t.Error("minted token should validate alongside the static one")
	}
	if _, ok := s.Validate(raw); !ok {
		t.Error("static token should survive the Mint-driven persist/reload")
	}
	// The static entry must never be persisted into tokens.json.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Validate(raw); ok {
		t.Error("a fresh store (no SetStaticToken) must NOT accept the static token — it lives in config, not on disk")
	}
	if _, ok := s2.Validate(minted); !ok {
		t.Error("minted token should load from disk in the fresh store")
	}
}
