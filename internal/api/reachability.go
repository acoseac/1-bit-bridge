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
// stale result that gets discarded.
const reachabilityProbeTimeout = 2 * time.Second

// reachabilityStatus is the cached per-root probe result.
//
// Reason is a STABLE machine-readable code, not free text. iOS maps the
// code to a localized UI string; introducing a new value is an additive
// wire change (existing iOS treats unknown reasons as a generic offline
// indicator). Current set:
//   - "offline":            timeout or transport-level failure
//   - "not_mounted":        os.ErrNotExist on the root path
//   - "permission_denied":  EACCES on the root path
type reachabilityStatus struct {
	Reachable bool
	Reason    string
	checkedAt time.Time
}

// reachabilityCache is a tiny TTL cache keyed on the ABSOLUTE resolved
// path of each library root. Keying on the absolute path (not the
// basename) avoids collisions in multi-root setups where two roots
// share a name but differ in path, or where a root is reconfigured to
// a new location with the same name.
type reachabilityCache struct {
	mu      sync.Mutex
	entries map[string]reachabilityStatus
}

func newReachabilityCache() *reachabilityCache {
	return &reachabilityCache{entries: make(map[string]reachabilityStatus)}
}

// probe returns the cached reachability for absRoot, refreshing on cache
// miss or staleness. The probe itself is bounded by
// reachabilityProbeTimeout — a network mount that doesn't respond
// within the budget reports "offline" rather than blocking the caller.
//
// Pass the request's context.Context so a client disconnect short-
// circuits the probe goroutine.
func (c *reachabilityCache) probe(ctx context.Context, absRoot string) reachabilityStatus {
	c.mu.Lock()
	if entry, ok := c.entries[absRoot]; ok && time.Since(entry.checkedAt) < reachabilityTTL {
		c.mu.Unlock()
		return entry
	}
	c.mu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, reachabilityProbeTimeout)
	defer cancel()

	result := make(chan reachabilityStatus, 1)
	go func() {
		_, err := os.Stat(absRoot)
		switch {
		case err == nil:
			result <- reachabilityStatus{Reachable: true, checkedAt: time.Now()}
		case errors.Is(err, fs.ErrPermission):
			result <- reachabilityStatus{Reachable: false, Reason: "permission_denied", checkedAt: time.Now()}
		case errors.Is(err, os.ErrNotExist):
			result <- reachabilityStatus{Reachable: false, Reason: "not_mounted", checkedAt: time.Now()}
		default:
			// Catch-all: I/O error, network filesystem returning a non-
			// classified error, kernel-level SMB / NFS faults. Generic
			// "offline" so iOS surfaces a network-style hint rather than
			// a confusing permission-or-existence one.
			result <- reachabilityStatus{Reachable: false, Reason: "offline", checkedAt: time.Now()}
		}
	}()

	var status reachabilityStatus
	select {
	case status = <-result:
	case <-probeCtx.Done():
		// Timeout (or upstream cancel) — record offline but DON'T cache
		// the cancellation case (the caller may have disconnected for
		// reasons unrelated to the root's health). Distinguish by
		// inspecting the parent ctx: only cache on a probe-timeout, not
		// on an upstream client-cancel.
		status = reachabilityStatus{Reachable: false, Reason: "offline", checkedAt: time.Now()}
		if ctx.Err() != nil {
			return status
		}
	}

	c.mu.Lock()
	c.entries[absRoot] = status
	c.mu.Unlock()
	return status
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
func (s *Server) probeAllRoots(ctx context.Context) []RootStatus {
	roots := s.resolver.Roots()
	out := make([]RootStatus, 0, len(roots))
	for _, abs := range roots {
		status := s.reachability.probe(ctx, abs)
		out = append(out, RootStatus{
			Name:      filepath.Base(abs),
			Reachable: status.Reachable,
			Reason:    status.Reason,
		})
	}
	return out
}
