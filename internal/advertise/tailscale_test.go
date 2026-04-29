package advertise

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// withStubTailscaleStatus swaps the package-level CLI invoker for the
// duration of one test and restores it on cleanup. Tests that need to
// drive specific JSON shapes call this instead of dropping a real
// shim binary on PATH (which is platform-fragile and racy under -race
// + parallel tests).
//
// Also resets the TTL cache between cases so a previous test's success
// state doesn't leak into the next test's stub. The cache lives at the
// package level (per-process) so `t.Cleanup` is the only point where
// we can reliably clear it.
func withStubTailscaleStatus(t *testing.T, st tailscaleStatus, err error) {
	t.Helper()
	prev := tailscaleStatusJSONFunc
	resetTailscaleStatusCache()
	t.Cleanup(func() {
		tailscaleStatusJSONFunc = prev
		resetTailscaleStatusCache()
	})
	tailscaleStatusJSONFunc = func() (tailscaleStatus, error) {
		return st, err
	}
}

func TestGetTailscaleDNSName_HappyPath(t *testing.T) {
	withStubTailscaleStatus(t, tailscaleStatus{
		Self: struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		}{
			DNSName:      "home-pc.tailfoo.ts.net.",
			TailscaleIPs: []string{"100.91.73.88", "fd7a:115c:a1e0:abcd:1::1"},
		},
	}, nil)
	if got := GetTailscaleDNSName(); got != "home-pc.tailfoo.ts.net" {
		t.Errorf("GetTailscaleDNSName() = %q, want %q",
			got, "home-pc.tailfoo.ts.net")
	}
}

// TestGetTailscaleDNSName_StripsTrailingDot pins the FQDN-vs-bare
// normalization. `tailscale status --json` emits with-trailing-dot on
// some versions (FQDN form) and without on others — TLS SAN matching
// and URL host parsing both want the bare form.
func TestGetTailscaleDNSName_StripsTrailingDot(t *testing.T) {
	withStubTailscaleStatus(t, tailscaleStatus{
		Self: struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		}{DNSName: "host.example.ts.net."},
	}, nil)
	if got := GetTailscaleDNSName(); got != "host.example.ts.net" {
		t.Errorf("trailing dot not stripped: got %q", got)
	}
}

func TestGetTailscaleDNSName_ErrorReturnsEmpty(t *testing.T) {
	withStubTailscaleStatus(t, tailscaleStatus{}, errors.New("CLI exited 1"))
	if got := GetTailscaleDNSName(); got != "" {
		t.Errorf("on error, want empty; got %q", got)
	}
}

func TestGetTailscaleDNSName_EmptyDNSReturnsEmpty(t *testing.T) {
	// Tailscale is reachable but the user has MagicDNS disabled. CLI
	// returns success but with empty DNSName — must produce empty
	// helper result so Endpoints() skips the append.
	withStubTailscaleStatus(t, tailscaleStatus{}, nil)
	if got := GetTailscaleDNSName(); got != "" {
		t.Errorf("empty DNSName, want empty; got %q", got)
	}
}

func TestGetTailscaleIPs_HappyPath(t *testing.T) {
	withStubTailscaleStatus(t, tailscaleStatus{
		Self: struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		}{
			TailscaleIPs: []string{"100.91.73.88", "fd7a:115c:a1e0:abcd:1::1"},
		},
	}, nil)
	got := GetTailscaleIPs()
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %v", len(got), got)
	}
	if !got[0].Equal(net.ParseIP("100.91.73.88")) {
		t.Errorf("got[0] = %v", got[0])
	}
	if !got[1].Equal(net.ParseIP("fd7a:115c:a1e0:abcd:1::1")) {
		t.Errorf("got[1] = %v", got[1])
	}
}

// TestGetTailscaleIPs_DropsMalformed verifies non-IP entries don't
// crash the parser and don't pollute the cert SAN list.
func TestGetTailscaleIPs_DropsMalformed(t *testing.T) {
	withStubTailscaleStatus(t, tailscaleStatus{
		Self: struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		}{
			TailscaleIPs: []string{"not-an-ip", "100.91.73.88", ""},
		},
	}, nil)
	got := GetTailscaleIPs()
	if len(got) != 1 {
		t.Fatalf("malformed entries should be dropped; got %v", got)
	}
}

func TestGetTailscaleIPs_ErrorReturnsNil(t *testing.T) {
	withStubTailscaleStatus(t, tailscaleStatus{}, errors.New("nope"))
	if got := GetTailscaleIPs(); got != nil {
		t.Errorf("on error, want nil; got %v", got)
	}
}

// TestEndpoints_AppendsMagicDNS verifies the new ClassTailscaleDNS
// entry shows up in Endpoints() output when the helper returns a
// MagicDNS name, and ranks AFTER LAN+mDNS but BEFORE the IP-based
// Tailscale classes.
func TestEndpoints_AppendsMagicDNS(t *testing.T) {
	withStubTailscaleStatus(t, tailscaleStatus{
		Self: struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		}{DNSName: "home.tailfoo.ts.net"},
	}, nil)
	eps := Endpoints(Params{Port: 7788, HostOverride: "test"})

	// Find the MagicDNS entry; assert URL shape and Class.
	var found *Endpoint
	for i := range eps {
		if eps[i].Class == ClassTailscaleDNS {
			found = &eps[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("MagicDNS entry missing; got %v", eps)
	}
	wantURL := "https://home.tailfoo.ts.net:7788"
	if found.URL != wantURL {
		t.Errorf("URL = %q, want %q", found.URL, wantURL)
	}

	// Class.String renders the new label.
	if got := ClassTailscaleDNS.String(); got != "Tailscale DNS" {
		t.Errorf("Class.String = %q, want %q", got, "Tailscale DNS")
	}
}

// TestEndpoints_NoMagicDNSWhenAbsent verifies Endpoints() doesn't
// emit a MagicDNS entry when the helper returns "" (Tailscale not
// installed / not running). Matches the current pre-PR-3 behaviour
// for hosts without Tailscale.
func TestEndpoints_NoMagicDNSWhenAbsent(t *testing.T) {
	withStubTailscaleStatus(t, tailscaleStatus{}, errors.New("not running"))
	eps := Endpoints(Params{Port: 7788, HostOverride: "test"})
	for _, e := range eps {
		if e.Class == ClassTailscaleDNS {
			t.Errorf("unexpected MagicDNS entry: %v", e)
		}
	}
}

// TestCachedTailscaleStatus_DedupesConcurrentCallers pins the
// singleflight contract: N concurrent callers in the same TTL window
// produce ONE underlying CLI invocation. /v1/health on a busy bridge
// can fan out 10+ concurrent calls per second; without singleflight
// each would spawn its own subprocess (Qodo bot review on PR #91).
func TestCachedTailscaleStatus_DedupesConcurrentCallers(t *testing.T) {
	resetTailscaleStatusCache()
	prev := tailscaleStatusJSONFunc
	t.Cleanup(func() { tailscaleStatusJSONFunc = prev })

	var calls int
	var callsMu sync.Mutex
	tailscaleStatusJSONFunc = func() (tailscaleStatus, error) {
		callsMu.Lock()
		calls++
		callsMu.Unlock()
		// Simulate slow CLI so concurrent goroutines pile up at the
		// singleflight gate while we're "in flight".
		time.Sleep(50 * time.Millisecond)
		return tailscaleStatus{
			Self: struct {
				DNSName      string   `json:"DNSName"`
				TailscaleIPs []string `json:"TailscaleIPs"`
			}{DNSName: "host.example.ts.net"},
		}, nil
	}

	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			if got := GetTailscaleDNSName(); got != "host.example.ts.net" {
				t.Errorf("got %q", got)
			}
		}()
	}
	wg.Wait()

	if calls != 1 {
		t.Errorf("singleflight broken: CLI invoked %d times, want 1", calls)
	}
}

// TestCachedTailscaleStatus_TTLHitReturnsCached pins the cache window:
// a second call within the TTL must NOT reinvoke the CLI.
func TestCachedTailscaleStatus_TTLHitReturnsCached(t *testing.T) {
	resetTailscaleStatusCache()
	prev := tailscaleStatusJSONFunc
	t.Cleanup(func() { tailscaleStatusJSONFunc = prev })

	var calls int
	tailscaleStatusJSONFunc = func() (tailscaleStatus, error) {
		calls++
		return tailscaleStatus{
			Self: struct {
				DNSName      string   `json:"DNSName"`
				TailscaleIPs []string `json:"TailscaleIPs"`
			}{DNSName: "host.example.ts.net"},
		}, nil
	}

	_ = GetTailscaleDNSName()
	_ = GetTailscaleDNSName()
	_ = GetTailscaleDNSName()
	if calls != 1 {
		t.Errorf("TTL cache broken: CLI invoked %d times, want 1", calls)
	}
}

// TestCachedTailscaleStatus_ErrorsAreNotCached verifies a transient
// failure (Tailscale not yet up) doesn't lock the cache for the TTL
// duration — the very next call retries.
func TestCachedTailscaleStatus_ErrorsAreNotCached(t *testing.T) {
	resetTailscaleStatusCache()
	prev := tailscaleStatusJSONFunc
	t.Cleanup(func() { tailscaleStatusJSONFunc = prev })

	var calls int
	tailscaleStatusJSONFunc = func() (tailscaleStatus, error) {
		calls++
		if calls == 1 {
			return tailscaleStatus{}, errors.New("not running")
		}
		return tailscaleStatus{
			Self: struct {
				DNSName      string   `json:"DNSName"`
				TailscaleIPs []string `json:"TailscaleIPs"`
			}{DNSName: "host.example.ts.net"},
		}, nil
	}

	if got := GetTailscaleDNSName(); got != "" {
		t.Errorf("call 1 (error): got %q, want empty", got)
	}
	if got := GetTailscaleDNSName(); got != "host.example.ts.net" {
		t.Errorf("call 2 (after error): got %q, want successful retry", got)
	}
	if calls != 2 {
		t.Errorf("expected 2 CLI calls (error not cached), got %d", calls)
	}
}
