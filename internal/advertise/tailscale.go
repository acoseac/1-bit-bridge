// Tailscale MagicDNS / IP detection via the local `tailscale` CLI.
//
// Why the CLI and not the LocalAPI: the LocalAPI's Unix socket / named
// pipe path varies by platform and Tailscale version, and the
// tailscale.com/client/tailscale Go package would pull in a chunky
// dependency for two strings. Spawning the already-installed CLI is a
// best-effort short-circuit — if Tailscale isn't running, isn't
// installed, or the CLI returned within the timeout without a DNSName,
// the helpers return zero-values and the caller falls back to the
// interface-walk path that already classifies CGNAT IPs as
// `ClassTailscaleV4`.
package advertise

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// tailscaleCLITimeout caps a single CLI invocation. The iOS client's
// per-endpoint probe is 5s; the bridge-side detector must be well
// under that to keep `Endpoints()` responsive on hosts where Tailscale
// is installed but its sockets are slow (cold-boot, recent update).
const tailscaleCLITimeout = 1500 * time.Millisecond

// tailscaleStatusTTL is how long a successful `tailscale status --json`
// invocation is cached. /v1/health is on the request hot-path and
// re-runs `Endpoints()` (which calls `GetTailscaleDNSName`) per call;
// without a cache, every health probe spawns a CLI subprocess up to
// 1.5s long, which is a real availability hazard on hosts where
// Tailscale's sockets are slow or under high health-probe load (Qodo
// bot review on PR #91 round 1).
//
// 30s is slow enough that a Tailscale up/down rarely lags by more than
// half a minute (the deferred-preload-style window we already accept
// elsewhere) and fast enough that an iOS client paired mid-tailnet-up
// sees the new MagicDNS endpoint within a single 15s endpoint-poll
// cycle on average.
//
// Errors are NOT cached — when Tailscale isn't running today and comes
// up tomorrow, we want the very next call after the change to discover
// it. Caching errors would tie that latency to the TTL.
const tailscaleStatusTTL = 30 * time.Second

// tailscaleStatus is the minimal subset of `tailscale status --json`
// we consume. Only `Self.DNSName` and `Self.TailscaleIPs` are read;
// every other field in the wire payload is silently ignored.
type tailscaleStatus struct {
	Self struct {
		DNSName      string   `json:"DNSName"`
		TailscaleIPs []string `json:"TailscaleIPs"`
	} `json:"Self"`
}

// tailscaleStatusJSONFunc is the test seam for the CLI invocation.
// Production callers use `cachedTailscaleStatus` (which wraps
// `runTailscaleStatusJSON` with a TTL cache + singleflight); tests
// swap this in directly to bypass the cache and emit canned JSON.
var tailscaleStatusJSONFunc = runTailscaleStatusJSON

// tailscaleStatusCache memoises successful CLI results for the TTL
// window. Concurrent callers all read the same cached value — see
// `cachedTailscaleStatus` for the singleflight that prevents N
// concurrent /v1/health probes from each spawning a subprocess on a
// cold start.
//
// Errors are not cached: a transient failure (Tailscale not yet up)
// should be retried on the next call, not propagated for 30s.
var (
	tailscaleStatusMu      sync.Mutex
	tailscaleStatusCached  tailscaleStatus
	tailscaleStatusErr     error
	tailscaleStatusFetched time.Time
	// tailscaleStatusInflight is the singleflight gate. While a CLI
	// invocation is in flight, concurrent callers wait on this channel
	// instead of spawning their own subprocess.
	tailscaleStatusInflight chan struct{}
)

// cachedTailscaleStatus wraps `tailscaleStatusJSONFunc` (the CLI exec)
// with a 30 s TTL cache + singleflight. /v1/health calls `Endpoints()`
// per request, which calls `GetTailscaleDNSName` / `GetTailscaleIPs` —
// without this layer, each health probe under load can fan out to N
// concurrent `tailscale status --json` subprocesses, each up to 1.5 s
// long. The cache pins one CLI call per 30 s window per process.
//
// Tests bypass this layer entirely by reassigning
// `tailscaleStatusJSONFunc` directly — the cache only sits between
// production callers and the real CLI.
func cachedTailscaleStatus() (tailscaleStatus, error) {
	tailscaleStatusMu.Lock()
	if !tailscaleStatusFetched.IsZero() &&
		time.Since(tailscaleStatusFetched) < tailscaleStatusTTL &&
		tailscaleStatusErr == nil {
		// Cache hit on a successful previous fetch.
		st := tailscaleStatusCached
		tailscaleStatusMu.Unlock()
		return st, nil
	}
	if tailscaleStatusInflight != nil {
		// Another goroutine is already fetching; wait for it.
		ch := tailscaleStatusInflight
		tailscaleStatusMu.Unlock()
		<-ch
		// Re-acquire and read whatever the leader stored.
		tailscaleStatusMu.Lock()
		st, err := tailscaleStatusCached, tailscaleStatusErr
		tailscaleStatusMu.Unlock()
		return st, err
	}
	// We're the leader — fetch.
	ch := make(chan struct{})
	tailscaleStatusInflight = ch
	tailscaleStatusMu.Unlock()

	// Recover a panic in the fetch so the cleanup below (clearing
	// `tailscaleStatusInflight` + `close(ch)`) always runs — a panic would
	// otherwise leave the singleflight latch set and the channel unclosed,
	// hanging every future caller (/v1/health, cert rotation) forever. Convert
	// to an error so waiters wake with a failure instead of a crash. DeepSeek review.
	st, err := func() (st tailscaleStatus, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("tailscale status fetch panicked: %v", r)
			}
		}()
		return tailscaleStatusJSONFunc()
	}()

	tailscaleStatusMu.Lock()
	tailscaleStatusCached = st
	tailscaleStatusErr = err
	if err == nil {
		tailscaleStatusFetched = time.Now()
	} else {
		// Don't update fetchedAt on errors so the next call retries
		// immediately rather than waiting out the TTL.
		tailscaleStatusFetched = time.Time{}
	}
	tailscaleStatusInflight = nil
	tailscaleStatusMu.Unlock()
	close(ch)
	return st, err
}

// resetTailscaleStatusCache is a test-only helper that clears the
// cache between cases. Internal — exported via the test file's
// access to package internals.
func resetTailscaleStatusCache() {
	tailscaleStatusMu.Lock()
	defer tailscaleStatusMu.Unlock()
	tailscaleStatusCached = tailscaleStatus{}
	tailscaleStatusErr = nil
	tailscaleStatusFetched = time.Time{}
	// Also clear the singleflight gate. If a prior test left an in-flight
	// channel set, a subsequent cachedTailscaleStatus() would block on
	// that stale channel and receive the pre-reset result.
	tailscaleStatusInflight = nil
}

// GetTailscaleDNSName returns the host's MagicDNS name (e.g.
// `home-pc.tailfoo.ts.net`) or "" if Tailscale isn't running, isn't
// installed, or the CLI returned within `tailscaleCLITimeout` without
// a DNSName field.
//
// Trailing `.` (FQDN form) is stripped — `tailscale status --json`
// historically emits both forms across versions; the SAN / URL-host
// uses are happier with the bare form.
//
// Silent on every error path. Tailscale not being installed is the
// common case on a non-tailnet host, not an operator-visible failure;
// errors land at `.debug` only.
func GetTailscaleDNSName() string {
	st, err := cachedTailscaleStatus()
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSpace(st.Self.DNSName), ".")
}

// GetTailscaleIPs returns the host's Tailscale-assigned IPs from the
// same `tailscale status --json` payload that backs GetTailscaleDNSName.
// Used by the cert generator (PR 5) to seed SAN IPAddresses;
// complements (does not replace) the interface-walk path that already
// detects 100.x CGNAT addresses by IP-range membership.
//
// Returns nil on any error / when Tailscale isn't reachable.
func GetTailscaleIPs() []net.IP {
	st, err := cachedTailscaleStatus()
	if err != nil {
		return nil
	}
	out := make([]net.IP, 0, len(st.Self.TailscaleIPs))
	for _, raw := range st.Self.TailscaleIPs {
		if ip := net.ParseIP(strings.TrimSpace(raw)); ip != nil {
			out = append(out, ip)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// runTailscaleStatusJSON locates the `tailscale` binary and runs
// `status --json` against it. Return value is the parsed minimal
// payload or a non-nil error; callers treat any error as "Tailscale
// unreachable" and return zero-values.
//
// Search order:
//
//  1. PATH (covers the documented install on every platform)
//  2. macOS: `/Applications/Tailscale.app/Contents/MacOS/Tailscale`
//     — the Mac App Store / DMG install doesn't auto-link the CLI
//     symlink unless the operator runs `Tailscale > CLI` once.
//  3. Windows: `<ProgramFiles>\Tailscale\tailscale.exe` and
//     `<ProgramFiles(x86)>\Tailscale\tailscale.exe` — env-derived to
//     work on hosts that installed Windows on a non-default drive,
//     non-English locales (`C:\Programmes\…`), and 32-bit-on-64-bit
//     installs.
//
// Empty / missing env-vars in the Windows path are skipped (we never
// construct a path against `""`).
func runTailscaleStatusJSON() (tailscaleStatus, error) {
	bin, err := locateTailscaleBinary()
	if err != nil {
		return tailscaleStatus{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), tailscaleCLITimeout)
	defer cancel()

	// `--peers=false` skips the bulk of the JSON payload (every peer
	// node + its routes). We only read `Self.DNSName` and
	// `Self.TailscaleIPs`, both still present without peers. Smaller
	// payload → faster decode → less CPU per /v1/health probe (Gemini
	// bot review on PR #91 round 1). Available on all Tailscale CLI
	// versions back to 1.32 (2022); earlier installs are unsupported
	// upstream.
	out, err := exec.CommandContext(ctx, bin, "status", "--json", "--peers=false").Output()
	if err != nil {
		return tailscaleStatus{}, err
	}

	var st tailscaleStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return tailscaleStatus{}, err
	}
	return st, nil
}

// locateTailscaleBinary returns the first executable `tailscale` path
// found on disk, or an error if none are reachable.
//
// The `exec.LookPath` result is required to be an absolute path
// (Qodo bot review on PR #91 round 1, defense-in-depth). A
// PATH-relative resolution (`./tailscale`, or a user-writable PATH
// directory in a misconfigured deployment) would otherwise let an
// attacker stage a hostile binary the bridge then runs with its own
// privileges. Production service-manager configs never put `.` or
// user-writable dirs on PATH; this guard catches misconfigurations
// before they become a vulnerability.
func locateTailscaleBinary() (string, error) {
	if p, err := exec.LookPath("tailscale"); err == nil && filepath.IsAbs(p) {
		return p, nil
	}

	for _, candidate := range platformTailscaleFallbacks() {
		if candidate == "" {
			continue
		}
		// `os.Stat` is enough — exec.CommandContext will fail at
		// `Output()` if the file isn't actually executable, and we
		// surface that failure as "Tailscale unreachable" the same
		// as a missing binary. No need to mirror Unix mode bits here.
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("tailscale CLI not found")
}

// platformTailscaleFallbacks returns OS-specific candidate paths that
// don't appear on PATH by default. The macOS / Windows GUI installers
// land the binary outside PATH; on Linux the package manager always
// puts it on PATH so no fallback is needed.
func platformTailscaleFallbacks() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
		}
	case "windows":
		var out []string
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
			if dir := os.Getenv(env); dir != "" {
				out = append(out, filepath.Join(dir, "Tailscale", "tailscale.exe"))
			}
		}
		return out
	default:
		return nil
	}
}
