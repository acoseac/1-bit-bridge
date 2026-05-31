package api

import (
	"context"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// fakeRegistrar records UpsertDeviceRegistration calls for assertions.
type fakeRegistrar struct {
	mu    sync.Mutex
	calls [][3]string // {deviceToken, tokenID, name}
}

func (f *fakeRegistrar) UpsertDeviceRegistration(_ context.Context, deviceToken, tokenID, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [3]string{deviceToken, tokenID, name})
	return nil
}

func (f *fakeRegistrar) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func TestValidDeviceToken(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"deadbeef", true},
		{"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", true}, // 64-char iOS shape
		{"ABCDEF", false}, // uppercase rejected (hex.EncodeToString is lowercase)
		{"xyz123", false}, // non-hex
		{"dead beef", false},
		{string(make([]byte, maxDeviceTokenLen+1)), false}, // over-length (also non-hex)
	}
	for _, c := range cases {
		if got := validDeviceToken(c.in); got != c.want {
			t.Errorf("validDeviceToken(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestTouchDeviceDebounce verifies the in-memory debounce: a repeated
// (device,token) pair within the TTL hits the store once, while a bind
// change (new token for the same device) writes again immediately.
func TestTouchDeviceDebounce(t *testing.T) {
	fr := &fakeRegistrar{}
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	s := New(cfg, nil, nil, "fp").WithDeviceRegistrar(fr)

	ctx := context.Background()
	s.touchDevice(ctx, "dev1", "tok-a")
	s.touchDevice(ctx, "dev1", "tok-a") // debounced — same pair, within TTL
	s.touchDevice(ctx, "dev1", "tok-a")
	if got := fr.count(); got != 1 {
		t.Fatalf("same (device,token) within TTL: want 1 upsert, got %d", got)
	}

	// Bind change (re-pairing → new auth token for the same device) must
	// write immediately, bypassing the TTL.
	s.touchDevice(ctx, "dev1", "tok-b")
	if got := fr.count(); got != 2 {
		t.Fatalf("bind change: want 2 upserts, got %d", got)
	}

	// A different device is independent.
	s.touchDevice(ctx, "dev2", "tok-a")
	if got := fr.count(); got != 3 {
		t.Fatalf("new device: want 3 upserts, got %d", got)
	}

	// Header path always passes name="" — the store-side CASE guard
	// preserves any pairing-captured name.
	fr.mu.Lock()
	defer fr.mu.Unlock()
	for _, c := range fr.calls {
		if c[2] != "" {
			t.Errorf("header-path upsert sent non-empty name %q", c[2])
		}
	}
}
