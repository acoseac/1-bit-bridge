package atlasharvest

import (
	"path/filepath"
	"testing"
	"time"
)

// TestStateStore_AtlasCredential pins the premium-cover credential gate:
// a usable credential needs both a token AND a base URL AND a non-expired
// token; any gap returns ok=false so the premium fetch is skipped and the
// caller falls through to CAA.
func TestStateStore_AtlasCredential(t *testing.T) {
	newStore := func(t *testing.T) *StateStore {
		t.Helper()
		s, err := OpenStateStore(filepath.Join(t.TempDir(), "atlas-harvest.json"))
		if err != nil {
			t.Fatalf("OpenStateStore: %v", err)
		}
		return s
	}

	t.Run("no credential → ok=false", func(t *testing.T) {
		s := newStore(t)
		if _, _, ok := s.AtlasCredential(); ok {
			t.Error("ok=true with no credential provisioned")
		}
	})

	t.Run("token without base URL → ok=false", func(t *testing.T) {
		s := newStore(t)
		// SetCredential normalizes, so set the base empty directly via the
		// public path: a blank baseURL must not produce a usable credential.
		if err := s.SetCredential("tok", "", time.Time{}); err != nil {
			t.Fatalf("SetCredential: %v", err)
		}
		if _, _, ok := s.AtlasCredential(); ok {
			t.Error("ok=true with an empty base URL")
		}
	})

	t.Run("valid unexpired → ok=true with values", func(t *testing.T) {
		s := newStore(t)
		if err := s.SetCredential("tok123", "https://atlas.example/", time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("SetCredential: %v", err)
		}
		token, base, ok := s.AtlasCredential()
		if !ok {
			t.Fatal("ok=false for a valid unexpired credential")
		}
		if token != "tok123" {
			t.Errorf("token = %q, want tok123", token)
		}
		// SetCredential trims the trailing slash.
		if base != "https://atlas.example" {
			t.Errorf("base = %q, want https://atlas.example", base)
		}
	})

	t.Run("expired token → ok=false", func(t *testing.T) {
		s := newStore(t)
		if err := s.SetCredential("tok", "https://atlas.example", time.Now().Add(-time.Minute)); err != nil {
			t.Fatalf("SetCredential: %v", err)
		}
		if _, _, ok := s.AtlasCredential(); ok {
			t.Error("ok=true for an expired credential")
		}
	})

	t.Run("zero expiry (unknown) is treated as not-expired", func(t *testing.T) {
		s := newStore(t)
		if err := s.SetCredential("tok", "https://atlas.example", time.Time{}); err != nil {
			t.Fatalf("SetCredential: %v", err)
		}
		if _, _, ok := s.AtlasCredential(); !ok {
			t.Error("ok=false for a zero-expiry (unknown) credential; should be usable")
		}
	})
}
