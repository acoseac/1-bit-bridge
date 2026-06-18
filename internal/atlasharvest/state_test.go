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

// TestStateStore_PendingCovers pins the cover-harvest pending set: add starts at
// 0 attempts + dedups, a re-add doesn't reset progress, a resolved entry is
// removed, and a miss increments + drops at the cap. Persists across reopen.
func TestStateStore_PendingCovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.json")
	s, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddPendingCovers([]string{"a", "b", "a", ""}); err != nil {
		t.Fatal(err)
	}
	snap := s.PendingCoversSnapshot()
	if len(snap) != 2 || snap["a"] != 0 || snap["b"] != 0 {
		t.Fatalf("snapshot=%v, want {a:0,b:0}", snap)
	}
	// A miss bumps b; a re-add must not reset it.
	if err := s.SettlePendingCovers(nil, []string{"b"}, 6); err != nil {
		t.Fatal(err)
	}
	if got := s.PendingCoversSnapshot()["b"]; got != 1 {
		t.Fatalf("b attempts=%d, want 1", got)
	}
	_ = s.AddPendingCovers([]string{"b"})
	if got := s.PendingCoversSnapshot()["b"]; got != 1 {
		t.Errorf("re-add reset b's attempts to %d", got)
	}
	// A premium hit removes a.
	_ = s.SettlePendingCovers([]string{"a"}, nil, 6)
	if _, ok := s.PendingCoversSnapshot()["a"]; ok {
		t.Errorf("a not removed after a premium hit")
	}
	// Repeated misses drop b at the cap.
	for i := 0; i < 6; i++ {
		_ = s.SettlePendingCovers(nil, []string{"b"}, 6)
	}
	if _, ok := s.PendingCoversSnapshot()["b"]; ok {
		t.Errorf("b not dropped at the attempt cap")
	}
	// Persisted: reopen sees the (now empty) set.
	s2, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(s2.PendingCoversSnapshot()); n != 0 {
		t.Errorf("reopened pending set has %d entries, want 0", n)
	}
}
