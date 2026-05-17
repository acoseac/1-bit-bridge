package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// fakeTailscaleProvider is a `admin.TailscaleProvider` stub for tests
// that need to drive `reachableEndpoints`'s tsnet-mode advertising
// path against canned snapshots. Mirrors the `fakeManifestProvider`
// pattern in api_test.go.
//
// `calls` counts invocations of `Status()` so cache-TTL tests can
// assert that back-to-back `reachableEndpoints()` calls don't thrash
// the underlying provider — the api layer's 5s TTL cache should
// collapse them to a single Status() call within the window.
type fakeTailscaleProvider struct {
	snap  admin.TailscaleStatus
	calls int
}

func (f *fakeTailscaleProvider) Status() admin.TailscaleStatus {
	f.calls++
	return f.snap
}

func (f *fakeTailscaleProvider) RefreshNow(_ context.Context) admin.TailscaleStatus {
	return f.snap
}

// makeServerForEndpoints builds a minimal Server for the reachable-
// endpoints tests. cfg.Tailscale.Mode is the load-bearing knob; other
// fields default to harmless test values. The fakeManifestProvider /
// auth store / httptest server are NOT spun up — these tests exercise
// `reachableEndpoints` in isolation, not the HTTP handler.
func makeServerForEndpoints(t *testing.T, mode string, customEndpoints []string) *Server {
	t.Helper()
	cfg := &config.Config{
		ListenAddress:   ":7788",
		LibraryName:     "Test Library",
		CustomEndpoints: customEndpoints,
	}
	cfg.Tailscale.Mode = mode
	return &Server{
		cfgHolder: config.NewRuntimeConfig(cfg),
	}
}

// TestReachableEndpoints_TsnetRunningAddsMagicDNSAndIPs is the happy
// path: tsnet node is Running with a populated MagicDNSName + IPs.
// All three URL shapes appear; the class-stable sort lands
// ClassTailscaleDNS ahead of V4 ahead of V6.
func TestReachableEndpoints_TsnetRunningAddsMagicDNSAndIPs(t *testing.T) {
	s := makeServerForEndpoints(t, "tsnet", nil)
	s.tailscaleStatus = &fakeTailscaleProvider{
		snap: admin.TailscaleStatus{
			BackendState: "Running",
			MagicDNSName: "bridge.example.ts.net",
			TailscaleIPs: []string{"100.64.0.5", "fd7a:115c::1"},
		},
	}
	eps := s.reachableEndpoints()

	gotDNS := contains(eps, "https://bridge.example.ts.net:7788")
	gotV4 := contains(eps, "https://100.64.0.5:7788")
	gotV6 := contains(eps, "https://[fd7a:115c::1]:7788")
	if !gotDNS || !gotV4 || !gotV6 {
		t.Fatalf("want all three tsnet URLs; got=%v", eps)
	}

	// Class-stable sort: ClassTailscaleDNS(3) < ClassTailscaleV4(4) <
	// ClassTailscaleV6(5). Verify by index.
	dnsIdx := indexOf(eps, "https://bridge.example.ts.net:7788")
	v4Idx := indexOf(eps, "https://100.64.0.5:7788")
	v6Idx := indexOf(eps, "https://[fd7a:115c::1]:7788")
	if dnsIdx >= v4Idx || v4Idx >= v6Idx {
		t.Errorf("class-stable order broken: dns=%d v4=%d v6=%d in %v",
			dnsIdx, v4Idx, v6Idx, eps)
	}
}

// TestReachableEndpoints_TsnetRunningStripsTrailingDot pins the
// FQDN-vs-bare-form normalization. Upstream `Self.DNSName` arrives
// trailing-dot on some Tailscale versions; URL/SAN consumers want
// the bare form.
func TestReachableEndpoints_TsnetRunningStripsTrailingDot(t *testing.T) {
	s := makeServerForEndpoints(t, "tsnet", nil)
	s.tailscaleStatus = &fakeTailscaleProvider{
		snap: admin.TailscaleStatus{
			BackendState: "Running",
			MagicDNSName: "bridge.example.ts.net.", // ← trailing dot
		},
	}
	eps := s.reachableEndpoints()
	if !contains(eps, "https://bridge.example.ts.net:7788") {
		t.Errorf("trailing dot not stripped; got=%v", eps)
	}
	for _, u := range eps {
		if strings.Contains(u, "ts.net.:") {
			t.Errorf("FQDN dot leaked into URL: %q", u)
		}
	}
}

// TestReachableEndpoints_TsnetRunningButEmptyMagicDNSSkipsIPs gates
// IP-only advertising on a populated MagicDNSName. Rationale: the LE
// cert covers the magic-DNS hostname only — IP-only URLs would dial
// with an IP-literal SNI that the cert can't satisfy, surfacing the
// same TLS error class PR #267 just fixed.
func TestReachableEndpoints_TsnetRunningButEmptyMagicDNSSkipsIPs(t *testing.T) {
	s := makeServerForEndpoints(t, "tsnet", nil)
	s.tailscaleStatus = &fakeTailscaleProvider{
		snap: admin.TailscaleStatus{
			BackendState: "Running",
			MagicDNSName: "", // ← MagicDNS disabled
			TailscaleIPs: []string{"100.64.0.5"},
		},
	}
	eps := s.reachableEndpoints()
	if contains(eps, "https://100.64.0.5:7788") {
		t.Errorf("IPs advertised without MagicDNSName; got=%v", eps)
	}
}

// TestReachableEndpoints_TsnetPreAuthSkipsAllTailscale covers every
// pre-Running BackendState. Each must produce zero tsnet URLs — the
// LE cert isn't being served yet, advertising would reintroduce the
// TLS error.
func TestReachableEndpoints_TsnetPreAuthSkipsAllTailscale(t *testing.T) {
	for _, state := range []string{"NoState", "", "NeedsLogin", "NeedsMachineAuth", "Stopped", "Starting"} {
		t.Run("state="+state, func(t *testing.T) {
			s := makeServerForEndpoints(t, "tsnet", nil)
			s.tailscaleStatus = &fakeTailscaleProvider{
				snap: admin.TailscaleStatus{
					BackendState: state,
					MagicDNSName: "bridge.example.ts.net",
					TailscaleIPs: []string{"100.64.0.5", "fd7a:115c::1"},
				},
			}
			eps := s.reachableEndpoints()
			if contains(eps, "https://bridge.example.ts.net:7788") ||
				contains(eps, "https://100.64.0.5:7788") ||
				contains(eps, "https://[fd7a:115c::1]:7788") {
				t.Errorf("pre-auth state %q must not advertise tsnet; got=%v", state, eps)
			}
		})
	}
}

// TestReachableEndpoints_TsnetNilProviderSafe — when the provider isn't
// wired (test harnesses, pre-this-PR bridges via the deferred-setter
// path) `reachableEndpoints` must not panic and must produce the same
// output as the host-network-only path.
func TestReachableEndpoints_TsnetNilProviderSafe(t *testing.T) {
	s := makeServerForEndpoints(t, "tsnet", nil)
	// Deliberately leave s.tailscaleStatus nil.
	eps := s.reachableEndpoints()
	for _, u := range eps {
		if strings.Contains(u, ".ts.net") {
			t.Errorf("nil provider produced tsnet URL: %q (full=%v)", u, eps)
		}
	}
}

// TestReachableEndpoints_CLIModeAdvertisesWhenCertPresent — `cli` mode
// now flows through the same provider abstraction as tsnet mode
// (PR refactor/cli-mode-advertise-via-provider). The gate differs:
// cli uses `CertPresent` (the LE cert is on disk under
// `data/tailscale/lecert/...`) instead of `BackendState == "Running"`
// (which is a tsnet-internal concept never populated in cli mode).
func TestReachableEndpoints_CLIModeAdvertisesWhenCertPresent(t *testing.T) {
	s := makeServerForEndpoints(t, "cli", nil)
	s.tailscaleStatus = &fakeTailscaleProvider{
		snap: admin.TailscaleStatus{
			CertPresent:  true,
			MagicDNSName: "host.tailnet.ts.net",
			TailscaleIPs: []string{"100.64.0.5", "fd7a:115c::1"},
		},
	}
	eps := s.reachableEndpoints()
	if !contains(eps, "https://host.tailnet.ts.net:7788") {
		t.Errorf("cli mode + CertPresent should advertise MagicDNS URL; got=%v", eps)
	}
	if !contains(eps, "https://100.64.0.5:7788") {
		t.Errorf("cli mode + CertPresent should advertise tailnet v4 IP; got=%v", eps)
	}
	if !contains(eps, "https://[fd7a:115c::1]:7788") {
		t.Errorf("cli mode + CertPresent should advertise tailnet v6 IP; got=%v", eps)
	}
}

// TestReachableEndpoints_CLIModeSkipsWhenCertMissing — cli mode's
// gate is `CertPresent`. Without the LE cert on disk the SNI switcher
// would fall through to the self-signed cert on any `.ts.net` SNI;
// iOS skips fingerprint pinning on `.ts.net` and trips ATS on the
// self-signed chain. Advertising in that state surfaces the same
// TLS-error class that PR #267 closed for tsnet — skip it.
func TestReachableEndpoints_CLIModeSkipsWhenCertMissing(t *testing.T) {
	s := makeServerForEndpoints(t, "cli", nil)
	s.tailscaleStatus = &fakeTailscaleProvider{
		snap: admin.TailscaleStatus{
			CertPresent:  false, // ← LE cert hasn't been minted yet
			MagicDNSName: "host.tailnet.ts.net",
			TailscaleIPs: []string{"100.64.0.5"},
		},
	}
	eps := s.reachableEndpoints()
	for _, u := range eps {
		if strings.Contains(u, ".ts.net") {
			t.Errorf("cli mode + !CertPresent must not advertise MagicDNS URL; got %q in %v", u, eps)
		}
		if strings.Contains(u, "100.64.") || strings.Contains(u, "fd7a:115c") {
			t.Errorf("cli mode + !CertPresent must not advertise tailnet IP; got %q in %v", u, eps)
		}
	}
}

// TestReachableEndpoints_DisabledModeSkipsTsnetAdvertising — same as
// cli mode but for the explicit `disabled` setting.
func TestReachableEndpoints_DisabledModeSkipsTsnetAdvertising(t *testing.T) {
	s := makeServerForEndpoints(t, "disabled", nil)
	fake := &fakeTailscaleProvider{
		snap: admin.TailscaleStatus{
			BackendState: "Running",
			MagicDNSName: "bridge.example.ts.net",
		},
	}
	s.tailscaleStatus = fake
	_ = s.reachableEndpoints()
	if fake.calls != 0 {
		t.Errorf("disabled mode invoked provider %d time(s); want 0", fake.calls)
	}
}

// TestReachableEndpoints_TsnetTTLCacheCoalescesProbes is the critical
// DoS-defence pin. /v1/health is unauthenticated; if the cache breaks,
// a LAN flood would multiply directly onto the tsnet LocalClient IPC
// channel. Two back-to-back reachableEndpoints() calls must invoke
// `Status()` exactly once (the cache fields default to zero, so the
// first call populates and the second within TTL re-uses).
func TestReachableEndpoints_TsnetTTLCacheCoalescesProbes(t *testing.T) {
	s := makeServerForEndpoints(t, "tsnet", nil)
	fake := &fakeTailscaleProvider{
		snap: admin.TailscaleStatus{
			BackendState: "Running",
			MagicDNSName: "bridge.example.ts.net",
		},
	}
	s.tailscaleStatus = fake
	_ = s.reachableEndpoints()
	_ = s.reachableEndpoints()
	if fake.calls != 1 {
		t.Errorf("cache failed to coalesce: got %d calls, want 1", fake.calls)
	}
	// Force cache expiry by reaching into the field (same-package
	// access — no new exported affordance needed; mirrors the
	// `friendlyErrorMessage` convention of using package-internal
	// surface for test gates).
	s.tailscaleStatusMu.Lock()
	s.tailscaleStatusFetched = time.Now().Add(-2 * tailscaleStatusTTL)
	s.tailscaleStatusMu.Unlock()
	_ = s.reachableEndpoints()
	if fake.calls != 2 {
		t.Errorf("post-TTL cache miss didn't re-probe: got %d calls, want 2", fake.calls)
	}
}

// TestReachableEndpoints_TsnetMagicDNSDupAgainstCustomKeepsTsnetClass
// is the class-ranking-bug regression test (the senior-review fix:
// sort-before-dedupe). An operator who hardcoded the magic-DNS URL
// into `customEndpoints` as a pre-fix workaround should see the URL
// emerge ranked as `ClassTailscaleDNS`, not `ClassCustom`. Validates
// the order of operations in `reachableEndpoints` (sort by class
// first, then dedupe with first-occurrence-wins).
func TestReachableEndpoints_TsnetMagicDNSDupAgainstCustomKeepsTsnetClass(t *testing.T) {
	const magicURL = "https://bridge.example.ts.net:7788"
	s := makeServerForEndpoints(t, "tsnet", []string{magicURL})
	s.tailscaleStatus = &fakeTailscaleProvider{
		snap: admin.TailscaleStatus{
			BackendState: "Running",
			MagicDNSName: "bridge.example.ts.net",
		},
	}
	eps := s.reachableEndpoints()

	// URL appears exactly once.
	occurrences := 0
	for _, u := range eps {
		if u == magicURL {
			occurrences++
		}
	}
	if occurrences != 1 {
		t.Errorf("dedup broke: URL appears %d times in %v", occurrences, eps)
	}

	// The kept instance should rank BEFORE any other custom URL,
	// which is the structural proof that it kept the tsnet class
	// (ClassTailscaleDNS=3) rather than ClassCustom=7. We add a
	// second custom URL that would NOT be deduped to anchor the
	// position check.
	s.cfgHolder.Store(applyCustomEndpoints(s.cfgHolder.Load(),
		[]string{magicURL, "https://anchor.example.com:7788"}))
	eps = s.reachableEndpoints()
	magicIdx := indexOf(eps, magicURL)
	anchorIdx := indexOf(eps, "https://anchor.example.com:7788")
	if anchorIdx < 0 {
		t.Fatalf("anchor URL missing: %v", eps)
	}
	if magicIdx >= anchorIdx {
		t.Errorf("magic-DNS URL did not rank ahead of pure-custom URL: magic=%d anchor=%d in %v",
			magicIdx, anchorIdx, eps)
	}
}

// --- small helpers ---

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func indexOf(haystack []string, needle string) int {
	for i, h := range haystack {
		if h == needle {
			return i
		}
	}
	return -1
}

// applyCustomEndpoints returns a copy of cfg with CustomEndpoints
// swapped in. RuntimeConfig stores by value so we can't poke the live
// pointer; instead build a fresh `*config.Config` and `Store` it on
// the holder.
func applyCustomEndpoints(cfg *config.Config, eps []string) *config.Config {
	clone := *cfg
	clone.CustomEndpoints = eps
	return &clone
}
