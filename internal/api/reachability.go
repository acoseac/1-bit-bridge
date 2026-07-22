package api

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// reachabilityTTL is how long a probe result is reused before refreshing.
// 5 s matches the iOS-side BridgeEndpointSelector cache and keeps /v1/list
// responsive under the steady-state poll cadence iOS imposes while the
// Folders tab is open.
const reachabilityTTL = 5 * time.Second

// reachabilityProbeTimeout caps a single os.Stat probe. Kernel-level SMB
// or NFS hangs can stall os.Stat for minutes on a flaky network mount;
// without a bound, a single hanging probe blocks the API goroutine that
// triggered it and under load becomes server-wide goroutine pressure.
// 2 s is generous for any reachable mount on a healthy network and short
// enough to keep the API responsive when one isn't. A stat call that
// outlives the timeout leaks its goroutine until the kernel returns
// (Go doesn't expose syscall cancellation) — accepted: the OS reaps
// the stuck stat eventually, the leaked goroutine's only state is a
// stale result that gets discarded. singleflight (see reachabilityCache
// docblock) keeps that leak to one goroutine per (root, stuck-window),
// not per concurrent caller.
const reachabilityProbeTimeout = 2 * time.Second

// statFunc is the os.Stat seam. Production code MUST NOT reassign it;
// only tests override it (and restore via t.Cleanup) so they can
// simulate a hard-mount NFS stat that never returns — the one failure
// mode the in-flight guard exists for, and one there is no portable
// way to stage with a real filesystem. Same convention as
// atomicwrite.renameFunc and tailscale.commandContext.
var statFunc = os.Stat

// reachabilityStatus is the cached per-root probe result.
//
// Reason is a STABLE machine-readable code, not free text. iOS maps the
// code to a localized UI string; introducing a new value is an additive
// wire change (existing iOS treats unknown reasons as a generic offline
// indicator). Current set:
//   - "offline":            timeout or transport-level failure
//   - "not_mounted":        os.ErrNotExist on the root path
//   - "permission_denied":  EACCES on the root path
//
// ModTime is the canonical mtime captured by the probe's os.Stat — only
// populated when Reachable is true. Reused by the list and stat
// handlers so they don't re-stat the root path on a healthy probe-hit;
// without this, every multi-root /v1/list call was doing two os.Stat
// calls per root (one in the probe, one in the handler immediately
// after) which doubles latency for network-mounted libraries.
type reachabilityStatus struct {
	Reachable bool
	Reason    string
	ModTime   time.Time
	checkedAt time.Time
}

// reachabilityCache is a tiny TTL cache keyed on the ABSOLUTE resolved
// path of each library root. Keying on the absolute path (not the
// basename) avoids collisions in multi-root setups where two roots
// share a name but differ in path, or where a root is reconfigured to
// a new location with the same name.
//
// **Thundering herd mitigation**: a `singleflight.Group` collapses
// concurrent in-flight probes for the same root onto a single
// goroutine. Without it, under a 1 Hz /v1/health poll from multiple
// iOS clients hitting the 5 s TTL boundary simultaneously, every
// caller racing through the stale-or-miss window would spawn its own
// probe goroutine. On a hung SMB/NFS mount that produces
// O(clients × offline-roots) goroutines all blocked on the kernel
// for up to 2 s — exactly the failure mode this code is supposed to
// protect against. The singleflight key is the absRoot string, so
// per-root staleness windows still go through their own probe (no
// cross-root serialisation), but per-root concurrent callers all
// wait on the first one's result.
// **Hung-mount goroutine guard**: singleflight bounds *concurrent*
// callers, but it does NOT bound goroutines across successive stale
// windows. On a hard-mount NFS (or an SMB share whose server vanished)
// `os.Stat` can block indefinitely — well past the 2 s budget. The
// flight then completes via the timeout branch and retires, while its
// stat goroutine stays parked in the kernel forever. Five seconds
// later the TTL lapses, the next /v1/health poll starts a fresh
// flight, and parks another one. At iOS's poll cadence that's on the
// order of 17k leaked goroutines a day — ~130 MB of stacks — on
// exactly the mount failure this cache exists to survive.
//
// `inflight` tracks roots whose stat goroutine has not yet returned.
// While one is parked we refuse to launch another and serve the
// offline verdict directly. It is self-healing: whenever the kernel
// finally releases the stat, the goroutine clears its own entry and
// the next lapsed-TTL probe re-tests the mount for real.
type reachabilityCache struct {
	mu       sync.Mutex
	entries  map[string]reachabilityStatus
	inflight map[string]bool
	group    singleflight.Group
}

func newReachabilityCache() *reachabilityCache {
	return &reachabilityCache{
		entries:  make(map[string]reachabilityStatus),
		inflight: make(map[string]bool),
	}
}

// probe returns the cached reachability for absRoot, refreshing on cache
// miss or staleness. The probe itself is bounded by
// reachabilityProbeTimeout — a network mount that doesn't respond
// within the budget reports "offline" rather than blocking the caller.
//
// Concurrent calls for the same absRoot are de-duplicated via the
// singleflight group: only one probe goroutine runs at a time per
// root, regardless of how many callers race through the cache miss.
//
// Pass the request's context.Context so a client disconnect short-
// circuits the probe goroutine.
//
// Nil receiver is safe: returns Reachable=true with a generic
// reason-less status so test harnesses that construct &Server{...}
// without going through New() don't panic. Production callers always
// go through New() which initialises the cache.
func (c *reachabilityCache) probe(ctx context.Context, absRoot string) reachabilityStatus {
	if c == nil {
		return reachabilityStatus{Reachable: true, checkedAt: time.Now()}
	}
	c.mu.Lock()
	if entry, ok := c.entries[absRoot]; ok && time.Since(entry.checkedAt) < reachabilityTTL {
		c.mu.Unlock()
		return entry
	}
	c.mu.Unlock()

	// singleflight collapses concurrent callers onto one probe goroutine.
	// The result is shared by every caller that arrived during the
	// in-flight window.
	v, _, _ := c.group.Do(absRoot, func() (interface{}, error) {
		return c.probeLocked(ctx, absRoot), nil
	})
	return v.(reachabilityStatus)
}

// probeLocked is the actual stat-and-classify body. Runs inside the
// singleflight closure so only one copy runs per (absRoot, stale-window).
//
// The probe deliberately DETACHES from the caller's ctx
// (context.WithoutCancel): the singleflight result is shared by every
// caller that joined the flight, so the first caller hanging up
// mid-probe must not turn into a synthesized "offline" verdict handed
// to the healthy callers that are still waiting. The 2 s probe timeout
// alone bounds the work; a timeout-fired "offline" IS the root's real
// state and is cached as such.
func (c *reachabilityCache) probeLocked(ctx context.Context, absRoot string) reachabilityStatus {
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reachabilityProbeTimeout)
	defer cancel()

	// Refuse to stack a second stat on a root whose previous one is
	// still parked in the kernel (see the `inflight` note on the struct).
	// Serving a freshly-stamped offline verdict — rather than the stale
	// entry as-is — is what makes the TTL actually suppress the next
	// poll; returning the old timestamp would re-enter this path on
	// every request.
	c.mu.Lock()
	if c.inflight[absRoot] {
		status := reachabilityStatus{Reachable: false, Reason: "offline", checkedAt: time.Now()}
		c.entries[absRoot] = status
		c.mu.Unlock()
		return status
	}
	c.inflight[absRoot] = true
	c.mu.Unlock()

	type probeResult struct {
		info os.FileInfo
		err  error
	}
	// Buffered so the goroutine can always publish and exit, even when
	// the timeout branch already abandoned this channel.
	resultCh := make(chan probeResult, 1)
	go func() {
		info, err := statFunc(absRoot)
		c.mu.Lock()
		delete(c.inflight, absRoot)
		c.mu.Unlock()
		resultCh <- probeResult{info: info, err: err}
	}()

	var status reachabilityStatus
	select {
	case pr := <-resultCh:
		status = classifyProbeResult(pr.info, pr.err)
	case <-probeCtx.Done():
		// Only the probe timeout can fire here (the parent's cancel no
		// longer propagates) — a >2 s stat IS an offline-class verdict
		// for a network mount, so caching it is correct.
		status = reachabilityStatus{Reachable: false, Reason: "offline", checkedAt: time.Now()}
	}

	c.mu.Lock()
	c.entries[absRoot] = status
	c.mu.Unlock()
	return status
}

// classifyProbeResult maps an os.Stat outcome to a reachabilityStatus.
// Pure function; exported for testability and to keep the select
// branches in probeLocked readable.
func classifyProbeResult(info os.FileInfo, err error) reachabilityStatus {
	now := time.Now()
	switch {
	case err == nil:
		return reachabilityStatus{Reachable: true, ModTime: info.ModTime().UTC(), checkedAt: now}
	case errors.Is(err, fs.ErrPermission):
		return reachabilityStatus{Reachable: false, Reason: "permission_denied", checkedAt: now}
	case errors.Is(err, os.ErrNotExist):
		return reachabilityStatus{Reachable: false, Reason: "not_mounted", checkedAt: now}
	default:
		// Catch-all: I/O error, network filesystem returning a non-
		// classified error, kernel-level SMB / NFS faults. Generic
		// "offline" so iOS surfaces a network-style hint rather than
		// a confusing permission-or-existence one.
		return reachabilityStatus{Reachable: false, Reason: "offline", checkedAt: now}
	}
}

// matchesRoot returns the absolute root path if clientPath identifies a
// configured root mount, or "" if it doesn't. A root match means:
//
//   - Single-root mode: clientPath is empty, "/" or "." (normalises to "").
//   - Multi-root mode: clientPath is exactly one segment matching a root's
//     basename. Descendant paths ("Music/Album/...") return "" — those
//     resolve through the normal resolver path.
//
// Pure function of the configured roots; called from /v1/stat handler
// to decide whether a 404 on the resolver path should be promoted to a
// structured "library offline" response instead.
func (s *Server) matchesRoot(clientPath string) string {
	roots := s.resolver.Roots()
	if len(roots) == 0 {
		return ""
	}
	clean := path.Clean(clientPath)
	if clean == "." {
		clean = ""
	}
	clean = strings.TrimPrefix(clean, "/")
	if len(roots) == 1 {
		if clean == "" {
			return roots[0]
		}
		return ""
	}
	// Multi-root: only single-segment paths match a root.
	if clean == "" || strings.Contains(clean, "/") {
		return ""
	}
	for _, root := range roots {
		if filepath.Base(root) == clean {
			return root
		}
	}
	return ""
}

// probeAllRoots returns per-root reachability for every configured root,
// in the same order as resolver.Roots(). Used by /v1/health to give iOS
// a single-call view of which libraries are online.
//
// Probes run in parallel — sequential probing would make /v1/health
// latency sum across every offline root (up to N × 2 s on a host with
// multiple hung mounts). The bounded fan-out is fine: probes already
// share the singleflight group, so concurrent callers don't multiply
// goroutines per root.
func (s *Server) probeAllRoots(ctx context.Context) []RootStatus {
	roots := s.resolver.Roots()
	if len(roots) == 0 {
		return nil
	}
	out := make([]RootStatus, len(roots))
	var wg sync.WaitGroup
	for i, abs := range roots {
		wg.Add(1)
		go func(i int, abs string) {
			defer wg.Done()
			status := s.reachability.probe(ctx, abs)
			out[i] = RootStatus{
				Name:      filepath.Base(abs),
				Reachable: status.Reachable,
				Reason:    status.Reason,
			}
		}(i, abs)
	}
	wg.Wait()
	return out
}
